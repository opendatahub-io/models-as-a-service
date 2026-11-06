package subscription //nolint:testpackage // Tests exercise unexported guardrail resolver helpers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// resourceCache is a namespace/name lookup over test resources.
type resourceCache map[string]*unstructured.Unstructured

func (c resourceCache) Get(namespace, name string) (*unstructured.Unstructured, error) {
	obj, ok := c[namespace+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "test", Resource: "resources"}, name)
	}
	return obj, nil
}

func TestGuardrailPlanResolver_ComposesAllScopesAndDeduplicatesChecks(t *testing.T) {
	tenantNamespace := "ai-tenants"
	targetNamespace := "tenant-a"
	modelNamespace := "models"

	tenant := newUnstructuredObject(tenantNamespace, "tenant-a", map[string]any{
		"spec":   map[string]any{"guardrails": attachmentList("tenant-policy", "tenant-policy")},
		"status": map[string]any{"tenantNamespace": targetNamespace},
	})
	tenantConfig := newUnstructuredObject(targetNamespace, defaultTenantConfigName, map[string]any{
		"spec": map[string]any{"guardrails": attachmentList("config-policy")},
	})
	subscription := newUnstructuredObject(targetNamespace, "subscription-a", map[string]any{
		"spec": map[string]any{
			"guardrails": attachmentList("subscription-policy"),
			"modelRefs": []any{map[string]any{
				"namespace":  modelNamespace,
				"name":       "qwen3",
				"guardrails": attachmentList("entry-policy"),
			}},
		},
	})
	model := newUnstructuredObject(modelNamespace, "qwen3", map[string]any{
		"spec":   map[string]any{"guardrails": attachmentList("model-policy")},
		"status": map[string]any{"resolvedTenantRef": "tenant-a"},
	})
	policies := newResourceCache(
		policyObject(targetNamespace, "tenant-policy", "tenant-check"),
		policyObject(targetNamespace, "config-policy", "config-check"),
		policyObject(targetNamespace, "subscription-policy", "subscription-check"),
		policyObject(targetNamespace, "entry-policy", "entry-check"),
		policyObject(modelNamespace, "model-policy", "prompt"),
	)

	resolver := NewGuardrailPlanResolver(
		tenantNamespace,
		"tenant-a",
		newResourceCache(tenant),
		newResourceCache(tenantConfig),
		policies,
	)

	selected, err := resolver.SelectedChecks(context.Background(), subscription, model)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{
		aiguardrail.CheckID("tenant-a", "tenant-policy", "tenant-check"):             true,
		aiguardrail.CheckID("tenant-a", "config-policy", "config-check"):             true,
		aiguardrail.CheckID("tenant-a", "subscription-policy", "subscription-check"): true,
		aiguardrail.CheckID("tenant-a", "entry-policy", "entry-check"):               true,
		aiguardrail.CheckID("models", "model-policy", "prompt"):                      true,
	}, selected)
}

func TestGuardrailPlanResolver_EmptyPlanReturnsEmptyMap(t *testing.T) {
	tenant := newUnstructuredObject("ai-tenants", "tenant-a", map[string]any{
		"status": map[string]any{"tenantNamespace": "tenant-a"},
	})
	tenantConfig := newUnstructuredObject("tenant-a", defaultTenantConfigName, map[string]any{})
	subscription := newUnstructuredObject("tenant-a", "subscription-a", map[string]any{
		"spec": map[string]any{"modelRefs": []any{map[string]any{"namespace": "models", "name": "qwen3"}}},
	})
	model := newUnstructuredObject("models", "qwen3", map[string]any{
		"status": map[string]any{"resolvedTenantRef": "tenant-a"},
	})

	resolver := NewGuardrailPlanResolver(
		"ai-tenants",
		"tenant-a",
		newResourceCache(tenant),
		newResourceCache(tenantConfig),
		newResourceCache(),
	)

	for _, tc := range []struct {
		name   string
		getter ResourceGetter
	}{
		{name: "API available", getter: newResourceCache()},
		{name: "API unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver.policyGetter = tc.getter
			selected, err := resolver.SelectedChecks(context.Background(), subscription, model)
			require.NoError(t, err)
			require.NotNil(t, selected)
			require.Empty(t, selected)
		})
	}

	t.Run("attachment requires unavailable API", func(t *testing.T) {
		resolver.policyGetter = nil
		tenant.Object["spec"] = map[string]any{"guardrails": attachmentList("privacy")}
		selected, err := resolver.SelectedChecks(context.Background(), subscription, model)
		require.ErrorIs(t, err, aiguardrail.ErrReadFailure)
		require.Nil(t, selected)
	})
}

func TestGuardrailPlanResolver_RejectsModelForAnotherTenant(t *testing.T) {
	tenant := newUnstructuredObject("ai-tenants", "tenant-a", map[string]any{
		"status": map[string]any{"tenantNamespace": "tenant-a"},
	})
	subscription := newUnstructuredObject("tenant-a", "subscription-a", map[string]any{})
	model := newUnstructuredObject("models", "qwen3", map[string]any{
		"status": map[string]any{"resolvedTenantRef": "tenant-b"},
	})

	resolver := NewGuardrailPlanResolver(
		"ai-tenants",
		"tenant-a",
		newResourceCache(tenant),
		newResourceCache(),
		newResourceCache(),
	)

	_, err := resolver.SelectedChecks(context.Background(), subscription, model)
	require.ErrorContains(t, err, "not resolved to AITenant tenant-a")
}

func TestAttachments_RejectsInvalidChecksType(t *testing.T) {
	obj := newUnstructuredObject("tenant-a", "subscription-a", map[string]any{
		"spec": map[string]any{"guardrails": []any{map[string]any{
			"ref":    map[string]any{"name": "privacy"},
			"checks": "pii",
		}}},
	})

	result, err := attachments(obj)
	require.ErrorContains(t, err, "guardrail attachment 0 has invalid checks")
	require.Nil(t, result)
}

func TestSelector_ModelSelectionSerializesEmptyGuardrailChecks(t *testing.T) {
	subscription := selectionSubscription()
	model := newUnstructuredObject("models", "qwen3", map[string]any{})
	for _, requestedModel := range []string{"models/qwen3", "qwen3"} {
		t.Run(requestedModel, func(t *testing.T) {
			selector := NewSelector(
				logger.New(false),
				staticSubscriptionLister{items: []*unstructured.Unstructured{subscription}},
				staticModelLister{items: []*unstructured.Unstructured{model}},
				staticAccessChecker{allowed: map[authpolicy.ModelKey]bool{{Namespace: "models", Name: "qwen3"}: true}},
				WithGuardrailPlanResolver(staticGuardrailResolver{}),
			)

			response, err := selector.SelectWithContext(context.Background(), []string{"users"}, "", "", requestedModel)
			require.NoError(t, err)
			require.NotNil(t, response.SelectedGuardrailChecks)
			require.Empty(t, *response.SelectedGuardrailChecks)

			body, err := json.Marshal(response)
			require.NoError(t, err)
			require.Contains(t, string(body), `"selectedGuardrailChecks":{}`)
		})
	}
}

func TestSelector_ModelSelectionWithoutResolverOmitsGuardrailChecks(t *testing.T) {
	subscription := selectionSubscription()
	model := newUnstructuredObject("models", "qwen3", map[string]any{})
	selector := NewSelector(
		logger.New(false),
		staticSubscriptionLister{items: []*unstructured.Unstructured{subscription}},
		staticModelLister{items: []*unstructured.Unstructured{model}},
		staticAccessChecker{allowed: map[authpolicy.ModelKey]bool{{Namespace: "models", Name: "qwen3"}: true}},
	)

	response, err := selector.SelectWithContext(context.Background(), []string{"users"}, "", "", "models/qwen3")
	require.NoError(t, err)
	require.Nil(t, response.SelectedGuardrailChecks)

	body, err := json.Marshal(response)
	require.NoError(t, err)
	require.NotContains(t, string(body), "selectedGuardrailChecks")
}

func TestSelector_ModelByKeyUsesGetWhenAvailable(t *testing.T) {
	model := newUnstructuredObject("models", "qwen3", map[string]any{})
	selector := NewSelector(logger.New(false), staticSubscriptionLister{}, gettableModelLister{resourceCache: newResourceCache(model)}, nil)

	found, err := selector.modelByKey("models", "qwen3")
	require.NoError(t, err)
	require.Same(t, model, found)

	_, err = selector.modelByKey("models", "missing")
	require.True(t, apierrors.IsNotFound(err))
}

func TestSelector_ModelByKeyFallsBackToList(t *testing.T) {
	model := newUnstructuredObject("models", "qwen3", map[string]any{})
	selector := NewSelector(logger.New(false), staticSubscriptionLister{}, staticModelLister{items: []*unstructured.Unstructured{model}}, nil)

	found, err := selector.modelByKey("models", "qwen3")
	require.NoError(t, err)
	require.Same(t, model, found)

	_, err = selector.modelByKey("models", "missing")
	require.ErrorContains(t, err, "MaaSModelRef models/missing not found")
}

func TestModelEntrySelection_RejectsDuplicateEntries(t *testing.T) {
	subscription := newUnstructuredObject("tenant-a", "subscription-a", map[string]any{
		"spec": map[string]any{"modelRefs": []any{
			map[string]any{"namespace": "models", "name": "qwen3"},
			map[string]any{"namespace": "models", "name": "qwen3"},
		}},
	})

	_, err := modelEntrySelection(subscription, "models", "qwen3")
	require.ErrorContains(t, err, "duplicate model entry models/qwen3")
}

func TestHandler_SelectSubscription_GuardrailUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	subscription := selectionSubscription()
	model := newUnstructuredObject("models", "qwen3", map[string]any{})
	selector := NewSelector(
		logger.New(false),
		staticSubscriptionLister{items: []*unstructured.Unstructured{subscription}},
		staticModelLister{items: []*unstructured.Unstructured{model}},
		staticAccessChecker{allowed: map[authpolicy.ModelKey]bool{{Namespace: "models", Name: "qwen3"}: true}},
		WithGuardrailPlanResolver(failingGuardrailResolver{}),
	)
	router := gin.New()
	metrics := &rejectionRecorder{}
	router.POST("/subscriptions/select", NewHandler(logger.New(false), selector, metrics).SelectSubscription)

	body, err := json.Marshal(SelectRequest{Groups: []string{"users"}, Username: "alice", RequestedModel: "models/qwen3"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewReader(body))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var result SelectResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, "guardrail_unavailable", result.Error)
	require.Equal(t, "selected guardrails are unavailable", result.Message)
	require.NotContains(t, response.Body.String(), "test guardrail failure")
	require.Equal(t, []string{constant.RejectionGuardrailUnavailable}, metrics.rejections)
}

type rejectionRecorder struct {
	rejections []string
}

func (r *rejectionRecorder) RecordRejection(reason string) {
	r.rejections = append(r.rejections, reason)
}

type staticSubscriptionLister struct {
	items []*unstructured.Unstructured
}

func (l staticSubscriptionLister) List() ([]*unstructured.Unstructured, error) {
	return l.items, nil
}

type staticModelLister struct {
	items []*unstructured.Unstructured
}

func (l staticModelLister) List() ([]*unstructured.Unstructured, error) {
	return l.items, nil
}

// gettableModelLister fails on List so tests prove the Get path is used.
type gettableModelLister struct {
	resourceCache
}

func (gettableModelLister) List() ([]*unstructured.Unstructured, error) {
	return nil, errors.New("List must not be called when Get is available")
}

type staticAccessChecker struct {
	allowed map[authpolicy.ModelKey]bool
}

func (c staticAccessChecker) AuthorizedModels([]string, string) (map[authpolicy.ModelKey]bool, error) {
	return c.allowed, nil
}

type staticGuardrailResolver struct{}

func (staticGuardrailResolver) SelectedChecks(context.Context, *unstructured.Unstructured, *unstructured.Unstructured) (map[string]bool, error) {
	return map[string]bool{}, nil
}

type failingGuardrailResolver struct{}

func (failingGuardrailResolver) SelectedChecks(context.Context, *unstructured.Unstructured, *unstructured.Unstructured) (map[string]bool, error) {
	return nil, errors.New("test guardrail failure")
}

func newResourceCache(objects ...*unstructured.Unstructured) resourceCache {
	cache := make(resourceCache, len(objects))
	for _, obj := range objects {
		cache[obj.GetNamespace()+"/"+obj.GetName()] = obj
	}
	return cache
}

func newUnstructuredObject(namespace, name string, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
			"uid":       namespace + "-" + name,
		},
	}
	for key, value := range fields {
		obj[key] = value
	}
	return &unstructured.Unstructured{Object: obj}
}

func selectionSubscription() *unstructured.Unstructured {
	return newUnstructuredObject("tenant-a", "subscription-a", map[string]any{
		"spec": map[string]any{
			"owner":     map[string]any{"groups": []any{map[string]any{"name": "users"}}},
			"modelRefs": []any{map[string]any{"namespace": "models", "name": "qwen3"}},
		},
		"status": map[string]any{"phase": "Active"},
	})
}

func attachmentList(names ...string) []any {
	items := make([]any, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]any{"ref": map[string]any{"name": name}})
	}
	return items
}

func policyObject(namespace, name, check string) *unstructured.Unstructured {
	obj := newUnstructuredObject(namespace, name, map[string]any{
		"spec": map[string]any{"checks": []any{map[string]any{
			"name":   check,
			"phases": []any{"Input"},
		}}},
		"status": map[string]any{
			"bindingRevision": "binding-1",
			"conditions": []any{map[string]any{
				"type":               "Accepted",
				"status":             "True",
				"observedGeneration": int64(0),
			}},
		},
	})
	obj.SetGeneration(0)
	return obj
}
