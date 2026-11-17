package subscription_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// aliasedModelRef builds a MaaSModelRef with the given status.resolvedModelAlias.
func aliasedModelRef(t *testing.T, name, namespace, kind, alias string) *unstructured.Unstructured {
	t.Helper()
	ref := createMaaSModelRef(name, namespace, kind)
	require.NoError(t, unstructured.SetNestedField(ref.Object, alias, "status", "resolvedModelAlias"))
	return ref
}

func TestSelect_ModelDenialMessage(t *testing.T) {
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{{"name": "model-b", "namespace": "ns1"}})
	models := &fakeModelLister{items: []*unstructured.Unstructured{
		aliasedModelRef(t, "hidden-ref", "hidden-ns", "ExternalModel", "hidden-alias"),
	}}
	selector := subscription.NewSelector(logger.New(false), &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, models,
		&fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
			{Namespace: "ns1", Name: "model-a"}: true,
			{Namespace: "ns1", Name: "model-b"}: true,
		}})

	const generic = `the requested model is not available in subscription "sub1"`
	for model, want := range map[string]string{
		// Authorized, but not in the subscription: denied like an unauthorized model (CWE-203).
		"ns1/model-a": `model "ns1/model-a" is not available in subscription "sub1"`,
		// Echoes the name the caller sent, not the alias-resolved hidden-ns/hidden-ref.
		"hidden-alias": `model "hidden-alias" is not available in subscription "sub1"`,
		"publishers/ns1/models/Org Model_v1.5:latest": `model "publishers/ns1/models/Org Model_v1.5:latest" is not available in subscription "sub1"`,
		// Names outside the echo-safe allow-list are not repeated.
		"bad\x01model": generic, "bad model": generic, `bad"model`: generic, `bad\model`: generic, "bad\nmodel": generic,
		strings.Repeat("m", 254): generic,
	} {
		_, err := selector.Select([]string{"g1"}, "", "sub1", model)
		var denied *subscription.ModelAccessDeniedError
		require.ErrorAs(t, err, &denied, model)
		assert.Equal(t, want, err.Error())
	}
}

func TestHandler_SelectSubscription_ModelDenial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New(false)
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{{"name": "model-a", "namespace": "ns1"}})
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, nil,
		&fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{}})
	metrics := &mockMetricsRecorder{}
	router := gin.New()
	router.POST("/subscriptions/select", subscription.NewHandler(log, selector, metrics).SelectSubscription)

	response := postSelection(t, router, `{"username":"alice","groups":["g1"],"requestedSubscription":"sub1","requestedModel":"ns1/model-a"}`)

	assert.Equal(t, map[string]any{
		"error":         "access_denied",
		"message":       `Access denied: model "ns1/model-a" is not available in subscription "sub1"`,
		"accessAllowed": false, "phase": "", "ready": false,
	}, response)
	assert.Equal(t, []string{constant.RejectionUnauthorized}, metrics.rejections)
}

func TestSelect_BodyRoutedModel(t *testing.T) {
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
		{"name": "renamed-ref", "namespace": "llm"},
		{"name": "no-alias", "namespace": "llm"},
		{"name": "qwen", "namespace": "llm"},
		{"name": "orphan", "namespace": "llm"}, // no MaaSModelRef
	})
	models := &fakeModelLister{items: []*unstructured.Unstructured{
		aliasedModelRef(t, "renamed-ref", "llm", "ExternalModel", "gpt-renamed"),
		aliasedModelRef(t, "no-alias", "llm", "ExternalModel", ""),
		aliasedModelRef(t, "qwen", "llm", "LLMInferenceService", "Qwen/Qwen3-0.6B"),
	}}
	authorized := map[authpolicy.ModelKey]bool{}
	for _, name := range []string{"renamed-ref", "no-alias", "qwen", "orphan"} {
		authorized[authpolicy.ModelKey{Namespace: "llm", Name: name}] = true
	}
	selector := subscription.NewSelector(logger.New(false), &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, models,
		&fakeAccessChecker{authorized: authorized})

	for requestedModel, want := range map[string]*string{
		"llm/renamed-ref": new("gpt-renamed"),
		"gpt-renamed":     new("gpt-renamed"),
		"llm/no-alias":    new(""), // unknown alias: present but empty
		"llm/orphan":      new(""), // unknown kind: present but empty
		"llm/qwen":        nil,     // LLMInferenceService: routed by path
		"":                nil,
	} {
		for _, requestedSubscription := range []string{"sub1", ""} {
			response, err := selector.Select([]string{"g1"}, "", requestedSubscription, requestedModel)
			require.NoError(t, err)
			require.True(t, response.AccessAllowed)
			assert.Equal(t, want, response.BodyRoutedModel, "model %q, subscription %q", requestedModel, requestedSubscription)
		}
	}

	// Absent (nil) lets the gateway check pass; empty must be sent so it fails.
	omitted, err := json.Marshal(subscription.SelectResponse{})
	require.NoError(t, err)
	assert.NotContains(t, string(omitted), "bodyRoutedModel")
	empty, err := json.Marshal(subscription.SelectResponse{BodyRoutedModel: new("")})
	require.NoError(t, err)
	assert.Contains(t, string(empty), `"bodyRoutedModel":""`)
}
