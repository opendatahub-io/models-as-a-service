package subscriptionidentity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	subscriptionidentity "github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscriptionidentity"
)

type fakeObservationSession struct {
	observations  []api_keys.SubscriptionObservation
	heartbeat     bool
	resolved      []api_keys.ResolveSubscriptionIdentityRequest
	calls         []string
	backfills     []fakeSubscriptionKeyBackfill
	backfillErr   error
	absent        []string
	absences      []api_keys.SubscriptionAbsenceRequest
	retirements   []api_keys.SubscriptionObserverRetirement
	retireErr     error
	heartbeatErr  error
	reactivateErr error
	resolveErr    error
	retireFresh   []time.Duration
	retireStale   []time.Duration
	retireSettle  []time.Duration
	rejoinFresh   []time.Duration
	resolveErrs   map[string]error
	commitErr     error
	committed     bool
	rolledBack    bool
	result        api_keys.SubscriptionCleanupResult
}

type fakeSubscriptionKeyBackfill struct {
	IdentityID string
	Namespace  string
	Name       string
}

func (session *fakeObservationSession) CurrentObservations(context.Context) ([]api_keys.SubscriptionObservation, error) {
	return session.observations, nil
}

func (session *fakeObservationSession) Heartbeat(context.Context) error {
	session.heartbeat = true
	return session.heartbeatErr
}

func (session *fakeObservationSession) ReactivateRetiredObserver(_ context.Context, freshWithin time.Duration) error {
	session.calls = append(session.calls, "reactivate-retired-observer")
	session.rejoinFresh = append(session.rejoinFresh, freshWithin)
	return session.reactivateErr
}

func (session *fakeObservationSession) RetireStaleObservers(
	_ context.Context,
	freshWithin, staleAfter, settle time.Duration,
) ([]api_keys.SubscriptionObserverRetirement, error) {
	session.calls = append(session.calls, "retire-stale-observers")
	session.retireFresh = append(session.retireFresh, freshWithin)
	session.retireStale = append(session.retireStale, staleAfter)
	session.retireSettle = append(session.retireSettle, settle)
	if session.retireErr != nil {
		return nil, session.retireErr
	}
	return session.retirements, nil
}

func (session *fakeObservationSession) ResolveSubscriptionIdentity(_ context.Context, request api_keys.ResolveSubscriptionIdentityRequest) (string, error) {
	session.resolved = append(session.resolved, request)
	session.calls = append(session.calls, "resolve:"+request.Namespace+"/"+request.Name)
	if err, found := session.resolveErrs[request.Namespace+"/"+request.Name]; found {
		return "", err
	}
	if session.resolveErr != nil {
		return "", session.resolveErr
	}
	return "0b6fe901-7b52-4a78-9cbf-e398728ef67e", nil
}

func (session *fakeObservationSession) BackfillSubscriptionKeys(
	_ context.Context,
	identityID, namespace, name string,
) (int, error) {
	session.calls = append(session.calls, "backfill:"+namespace+"/"+name)
	session.backfills = append(session.backfills, fakeSubscriptionKeyBackfill{
		IdentityID: identityID,
		Namespace:  namespace,
		Name:       name,
	})
	if session.backfillErr != nil {
		return 0, session.backfillErr
	}
	return 1, nil
}

func (session *fakeObservationSession) MarkAbsent(
	_ context.Context,
	subscriptionID string,
	_ bool,
) error {
	session.absent = append(session.absent, subscriptionID)
	return nil
}

func (session *fakeObservationSession) RecordAbsenceAndCleanup(
	_ context.Context,
	request api_keys.SubscriptionAbsenceRequest,
	_ time.Duration,
	_ time.Duration,
) (api_keys.SubscriptionCleanupResult, int, error) {
	session.absences = append(session.absences, request)
	if session.result == "" {
		return api_keys.SubscriptionCleanupRetained, 0, nil
	}
	if session.result == api_keys.SubscriptionCleanupRetainedPolicy {
		return session.result, 0, nil
	}
	return session.result, 2, nil
}

func (session *fakeObservationSession) Commit() error {
	if session.commitErr != nil {
		return session.commitErr
	}
	session.committed = true
	return nil
}

func (session *fakeObservationSession) Rollback() error {
	session.rolledBack = true
	return nil
}

type fakeIdentityStore struct {
	session *fakeObservationSession
}

type fakeObserverMetrics struct {
	retirements []recordedObserverRetirement
	rejoins     []string
}

type recordedObserverRetirement struct {
	tenant         string
	result         string
	revokedAPIKeys int
}

func (metrics *fakeObserverMetrics) RecordSubscriptionObserverRetirement(tenant, result string, revokedAPIKeys int) {
	metrics.retirements = append(metrics.retirements, recordedObserverRetirement{
		tenant:         tenant,
		result:         result,
		revokedAPIKeys: revokedAPIKeys,
	})
}

func (metrics *fakeObserverMetrics) RecordSubscriptionObserverRejoin(tenant, result string) {
	metrics.rejoins = append(metrics.rejoins, tenant+":"+result)
}

type recordingDynamicClient struct {
	dynamic.Interface

	patchTypes         []types.PatchType
	patches            [][]byte
	subresources       [][]string
	conflictFirstPatch bool
	invalidFirstPatch  bool
	conflictIdentityID string
}

func (client *recordingDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingResource{
		NamespaceableResourceInterface: client.Interface.Resource(gvr),
		client:                         client,
	}
}

type recordingResource struct {
	dynamic.NamespaceableResourceInterface

	client *recordingDynamicClient
}

func (resource *recordingResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &namespacedRecordingResource{
		ResourceInterface: resource.NamespaceableResourceInterface.Namespace(namespace),
		client:            resource.client,
	}
}

type namespacedRecordingResource struct {
	dynamic.ResourceInterface

	client *recordingDynamicClient
}

func (resource *namespacedRecordingResource) Patch(
	ctx context.Context,
	name string,
	patchType types.PatchType,
	data []byte,
	options metav1.PatchOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	resource.client.patchTypes = append(resource.client.patchTypes, patchType)
	resource.client.patches = append(resource.client.patches, append([]byte(nil), data...))
	resource.client.subresources = append(resource.client.subresources, append([]string(nil), subresources...))
	if (resource.client.conflictFirstPatch || resource.client.invalidFirstPatch) && len(resource.client.patches) == 1 {
		conflict := resource.client.conflictFirstPatch
		resource.client.conflictFirstPatch = false
		resource.client.invalidFirstPatch = false
		current, err := resource.Get(ctx, name, metav1.GetOptions{}, subresources...)
		if err != nil {
			return nil, err
		}
		current.SetResourceVersion("rv-2")
		if resource.client.conflictIdentityID != "" {
			if err := unstructured.SetNestedField(
				current.Object,
				resource.client.conflictIdentityID,
				"status",
				"subscriptionIdentityId",
			); err != nil {
				return nil, err
			}
		}
		if _, err := resource.Update(ctx, current, metav1.UpdateOptions{}, subresources...); err != nil {
			return nil, err
		}
		if conflict {
			return nil, apierrors.NewConflict(schema.GroupResource{Group: "maas.opendatahub.io", Resource: "maassubscriptions"}, name, errors.New("concurrent status write"))
		}
		return nil, apierrors.NewInvalid(schema.GroupKind{Group: "maas.opendatahub.io", Kind: "MaaSSubscription"}, name, field.ErrorList{
			field.Invalid(field.NewPath("metadata", "resourceVersion"), "rv-1", "stale resource version"),
		})
	}
	return resource.Get(ctx, name, metav1.GetOptions{}, subresources...)
}

func (store *fakeIdentityStore) BeginSubscriptionObservation(context.Context, string, string) (api_keys.SubscriptionObservationSession, error) {
	return store.session, nil
}

func TestHandlerRecordAbsenceVerifiesObjectAndCommits(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("sub-delete")
	object.SetNamespace("subscriptions")
	object.SetUID("object-uid")
	now := metav1.Now()
	object.SetDeletionTimestamp(&now)
	require.NoError(t, unstructured.SetNestedField(object.Object, "identity-id", "status", "subscriptionIdentityId"))

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{result: api_keys.SubscriptionCleanupCleaned}
	handler := subscriptionidentity.NewHandler(
		logger.Development(),
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		30*time.Second,
		3*time.Minute,
	)

	router := gin.New()
	router.POST("/internal/v1/subscriptions/:namespace/:name/absence", handler.RecordAbsence)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/subscriptions/subscriptions/sub-delete/absence",
		strings.NewReader(`{"objectUID":"object-uid","subscriptionIdentityId":"identity-id"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.False(t, session.heartbeat, "absence requests must not refresh full-observation freshness")
	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	require.Len(t, session.absences, 1)
	assert.Equal(t, "subscriptions", session.absences[0].Namespace)
	assert.Equal(t, "sub-delete", session.absences[0].Name)
	assert.Equal(t, "object-uid", session.absences[0].ObjectUID)
	assert.Equal(t, "identity-id", session.absences[0].SubscriptionIdentityID)
	assert.Contains(t, response.Body.String(), `"result":"cleaned"`)
	assert.Contains(t, response.Body.String(), `"revokedAPIKeys":2`)
}

func TestHandlerRecordAbsenceRejectsStatusMismatch(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("sub-delete")
	object.SetNamespace("subscriptions")
	object.SetUID("object-uid")
	now := metav1.Now()
	object.SetDeletionTimestamp(&now)
	require.NoError(t, unstructured.SetNestedField(object.Object, "status-identity", "status", "subscriptionIdentityId"))

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{}
	handler := subscriptionidentity.NewHandler(
		logger.Development(),
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		30*time.Second,
		3*time.Minute,
	)

	router := gin.New()
	router.POST("/internal/v1/subscriptions/:namespace/:name/absence", handler.RecordAbsence)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/subscriptions/subscriptions/sub-delete/absence",
		strings.NewReader(`{"objectUID":"object-uid","subscriptionIdentityId":"request-identity"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Empty(t, session.absences)
}

func TestHandlerRecordAbsenceRetainsAPIKeys(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("sub-delete")
	object.SetNamespace("subscriptions")
	object.SetUID("object-uid")
	now := metav1.Now()
	object.SetDeletionTimestamp(&now)

	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{subscription.GVR(): "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{result: api_keys.SubscriptionCleanupRetainedPolicy}
	handler := subscriptionidentity.NewHandler(
		logger.Development(),
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		30*time.Second,
		3*time.Minute,
	)

	router := gin.New()
	router.POST("/internal/v1/subscriptions/:namespace/:name/absence", handler.RecordAbsence)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/subscriptions/subscriptions/sub-delete/absence",
		strings.NewReader(`{"objectUID":"object-uid","retainAPIKeys":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, session.absences, 1)
	assert.True(t, session.absences[0].RetainAPIKeys)
	assert.Contains(t, response.Body.String(), `"result":"retained_policy"`)
	assert.Contains(t, response.Body.String(), `"revokedAPIKeys":0`)
}

func TestHandlerRecordAbsenceReturnsConflictWhenCleanupIsBlocked(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("sub-delete")
	object.SetNamespace("subscriptions")
	object.SetUID("object-uid")
	now := metav1.Now()
	object.SetDeletionTimestamp(&now)

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{result: api_keys.SubscriptionCleanupBlockedEmpty}
	handler := subscriptionidentity.NewHandler(
		logger.Development(),
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		30*time.Second,
		3*time.Minute,
	)

	router := gin.New()
	router.POST("/internal/v1/subscriptions/:namespace/:name/absence", handler.RecordAbsence)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/subscriptions/subscriptions/sub-delete/absence",
		strings.NewReader(`{"objectUID":"object-uid"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.True(t, session.committed)
	assert.Contains(t, response.Body.String(), `"result":"blocked_empty"`)
}

func TestObserverRefreshListsAppliesStatusAndMarksAbsence(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("new-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("new-uid")
	object.SetResourceVersion("rv-1")

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient}
	session := &fakeObservationSession{
		observations: []api_keys.SubscriptionObservation{{
			SubscriptionID: "old-identity",
			Namespace:      "subscriptions",
			Name:           "old-sub",
			ObjectUID:      "old-uid",
		}},
	}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	)

	observer.Refresh(context.Background())

	assert.True(t, session.heartbeat)
	assert.True(t, session.committed)
	require.Len(t, session.resolved, 1)
	assert.Equal(t, "subscriptions", session.resolved[0].Namespace)
	assert.Equal(t, "new-sub", session.resolved[0].Name)
	assert.Equal(t, "new-uid", session.resolved[0].ObjectUID)
	assert.Equal(t, []string{"old-identity"}, session.absent)

	require.Len(t, client.patches, 1)
	assert.Equal(t, types.JSONPatchType, client.patchTypes[0])
	require.Len(t, client.subresources, 1)
	assert.Equal(t, []string{"status"}, client.subresources[0])
	var patch []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &patch))
	require.Len(t, patch, 3)
	assert.Equal(t, "test", patch[0]["op"])
	assert.Equal(t, "/metadata/uid", patch[0]["path"])
	assert.Equal(t, "new-uid", patch[0]["value"])
	assert.Equal(t, "test", patch[1]["op"])
	assert.Equal(t, "/metadata/resourceVersion", patch[1]["path"])
	assert.Equal(t, "rv-1", patch[1]["value"])
	assert.Equal(t, "add", patch[2]["op"])
	assert.Equal(t, "/status", patch[2]["path"])
	assert.Equal(t, map[string]any{"subscriptionIdentityId": "0b6fe901-7b52-4a78-9cbf-e398728ef67e"}, patch[2]["value"])
}

func TestObserverRefreshRetiresStaleObserversAfterSuccessfulResolution(t *testing.T) {
	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
	)
	session := &fakeObservationSession{
		retirements: []api_keys.SubscriptionObserverRetirement{{
			ObserverID:     "22222222-3333-4444-5555-666666666666",
			Result:         api_keys.SubscriptionCleanupCleaned,
			RevokedAPIKeys: 2,
		}},
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableStaleObserverRetirement(3*time.Minute, 7*24*time.Hour).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.heartbeat)
	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	require.Equal(t, []string{"retire-stale-observers"}, session.calls)
	require.Equal(t, []time.Duration{3 * time.Minute}, session.retireFresh)
	require.Equal(t, []time.Duration{7 * 24 * time.Hour}, session.retireStale)
	require.Equal(t, []recordedObserverRetirement{{
		tenant:         "tenant-a",
		result:         "cleaned",
		revokedAPIKeys: 2,
	}}, metrics.retirements)
}

func TestObserverRefreshSkipsStaleRetirementWhenResolutionFails(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("pending-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("pending-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{resolveErr: errors.New("identity pending")}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableStaleObserverRetirement(3*time.Minute, 7*24*time.Hour).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.heartbeat)
	assert.True(t, session.committed)
	assert.Empty(t, session.retireFresh)
	require.Equal(t, []recordedObserverRetirement{{tenant: "tenant-a", result: "blocked"}}, metrics.retirements)
}

func TestObserverRefreshRollsBackWhenStaleRetirementFails(t *testing.T) {
	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
	)
	session := &fakeObservationSession{retireErr: errors.New("retirement failed")}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableStaleObserverRetirement(3*time.Minute, 7*24*time.Hour).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.False(t, session.committed)
	assert.True(t, session.rolledBack)
	require.Equal(t, []recordedObserverRetirement{{tenant: "tenant-a", result: "failure"}}, metrics.retirements)
}

func TestObserverRefreshRetiresStaleObserversWhenIdentityIsOnlyPending(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("pending-retirement-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("pending-retirement-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{
		resolveErr:  api_keys.ErrSubscriptionIdentityPending,
		retirements: []api_keys.SubscriptionObserverRetirement{{Result: api_keys.SubscriptionCleanupRetained}},
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableStaleObserverRetirement(3*time.Minute, 7*24*time.Hour).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	require.Equal(t, []string{
		"resolve:subscriptions/pending-retirement-sub",
		"retire-stale-observers",
	}, session.calls)
	require.Equal(t, []recordedObserverRetirement{{tenant: "tenant-a", result: "retained"}}, metrics.retirements)
}

func TestObserverRefreshSafelyRejoinsRetiredObserver(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("rejoin-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("rejoin-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		observations: []api_keys.SubscriptionObservation{{
			SubscriptionID: "old-identity",
			Namespace:      "subscriptions",
			Name:           "old-sub",
			ObjectUID:      "old-uid",
		}},
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3 * time.Minute).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.heartbeat)
	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	require.Len(t, session.resolved, 1)
	assert.Equal(t, []string{"old-identity"}, session.absent)
	require.Equal(t, []time.Duration{3 * time.Minute}, session.rejoinFresh)
	require.Equal(t, []string{"resolve:subscriptions/rejoin-sub", "reactivate-retired-observer"}, session.calls)
	require.Equal(t, []string{"tenant-a:success"}, metrics.rejoins)
}

func TestObserverRefreshBlocksRejoinWhenResolutionFails(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("blocked-rejoin-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("blocked-rejoin-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		resolveErr:   errors.New("identity pending"),
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3 * time.Minute).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.False(t, session.committed)
	assert.True(t, session.rolledBack)
	assert.Empty(t, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:blocked"}, metrics.rejoins)
}

func TestObserverRefreshPreservesPendingRotationWhileBlockingRejoin(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("rotation-rejoin-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("rotation-rejoin-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		resolveErr:   api_keys.ErrSubscriptionIdentityPending,
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3 * time.Minute).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	assert.Empty(t, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:blocked"}, metrics.rejoins)
}

func TestObserverRefreshBlocksRejoinWhenResolutionHasMixedFailures(t *testing.T) {
	failedObject := &unstructured.Unstructured{}
	failedObject.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	failedObject.SetName("failed-sub")
	failedObject.SetNamespace("subscriptions")
	failedObject.SetUID("failed-uid")

	pendingObject := &unstructured.Unstructured{}
	pendingObject.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	pendingObject.SetName("pending-sub")
	pendingObject.SetNamespace("subscriptions")
	pendingObject.SetUID("pending-uid")

	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		failedObject,
		pendingObject,
	)
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		resolveErrs: map[string]error{
			"subscriptions/failed-sub":  errors.New("identity resolution failed"),
			"subscriptions/pending-sub": api_keys.ErrSubscriptionIdentityPending,
		},
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3 * time.Minute).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.False(t, session.committed)
	assert.True(t, session.rolledBack)
	assert.Empty(t, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:blocked"}, metrics.rejoins)
}

func TestObserverRefreshAppliesStatusWritesAfterPendingRotationCommit(t *testing.T) {
	recoveredObject := &unstructured.Unstructured{}
	recoveredObject.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	recoveredObject.SetName("recovered-sub")
	recoveredObject.SetNamespace("subscriptions")
	recoveredObject.SetUID("recovered-uid")
	recoveredObject.SetResourceVersion("rv-1")
	require.NoError(t, unstructured.SetNestedField(
		recoveredObject.Object,
		"old-identity",
		"status",
		"subscriptionIdentityId",
	))

	pendingObject := &unstructured.Unstructured{}
	pendingObject.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	pendingObject.SetName("pending-sub")
	pendingObject.SetNamespace("subscriptions")
	pendingObject.SetUID("pending-uid")

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		recoveredObject,
		pendingObject,
	)
	client := &recordingDynamicClient{Interface: baseClient}
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		resolveErrs: map[string]error{
			"subscriptions/pending-sub": api_keys.ErrSubscriptionIdentityPending,
		},
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3 * time.Minute).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	assert.False(t, session.rolledBack)
	assert.Empty(t, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:blocked"}, metrics.rejoins)
	require.Len(t, client.patches, 1)
	var patch []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &patch))
	require.Len(t, patch, 3)
	assert.Equal(t, "/metadata/uid", patch[0]["path"])
	assert.Equal(t, "recovered-uid", patch[0]["value"])
	assert.Equal(t, "/status/subscriptionIdentityId", patch[2]["path"])
	assert.Equal(t, "0b6fe901-7b52-4a78-9cbf-e398728ef67e", patch[2]["value"])
}

func TestObserverRefreshRecordsRejoinFailure(t *testing.T) {
	tests := []struct {
		name    string
		session *fakeObservationSession
	}{
		{
			name: "reactivation fails",
			session: &fakeObservationSession{
				heartbeatErr:  api_keys.ErrObserverRetired,
				reactivateErr: errors.New("reactivation failed"),
			},
		},
		{
			name: "commit fails",
			session: &fakeObservationSession{
				heartbeatErr: api_keys.ErrObserverRetired,
				commitErr:    errors.New("commit failed"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gvr := subscription.GVR()
			client := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
			)
			metrics := &fakeObserverMetrics{}
			observer := subscriptionidentity.NewObserver(
				&fakeIdentityStore{session: test.session},
				client,
				"subscriptions",
				"tenant-a",
				"11111111-2222-3333-4444-555555555555",
				time.Hour,
				30*time.Second,
				logger.Development(),
			).
				EnableObserverRejoin(3 * time.Minute).
				WithMetricsRecorder(metrics)

			observer.Refresh(context.Background())

			assert.False(t, test.session.committed)
			assert.True(t, test.session.rolledBack)
			require.Equal(t, []time.Duration{3 * time.Minute}, test.session.rejoinFresh)
			require.Equal(t, []string{"tenant-a:failure"}, metrics.rejoins)
		})
	}
}

func TestObserverRefreshBlocksRejoinWhenRejoinIsNotConfigured(t *testing.T) {
	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
	)
	session := &fakeObservationSession{heartbeatErr: api_keys.ErrObserverRetired}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.False(t, session.committed)
	assert.True(t, session.rolledBack)
	assert.Empty(t, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:blocked"}, metrics.rejoins)
}

func TestObserverRefreshRecordsRejoinFailureWhenRetirementFailsAfterReactivation(t *testing.T) {
	gvr := subscription.GVR()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
	)
	session := &fakeObservationSession{
		heartbeatErr: api_keys.ErrObserverRetired,
		retireErr:    errors.New("retirement failed"),
	}
	metrics := &fakeObserverMetrics{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).
		EnableObserverRejoin(3*time.Minute).
		EnableStaleObserverRetirement(3*time.Minute, 7*24*time.Hour).
		WithMetricsRecorder(metrics)

	observer.Refresh(context.Background())

	assert.False(t, session.committed)
	assert.True(t, session.rolledBack)
	require.Equal(t, []time.Duration{3 * time.Minute}, session.rejoinFresh)
	require.Equal(t, []string{"tenant-a:failure"}, metrics.rejoins)
	require.Equal(t, []recordedObserverRetirement{{tenant: "tenant-a", result: "failure"}}, metrics.retirements)
}

func TestObserverRefreshBackfillsKeysAndReportsMigrationReady(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("migration-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("migration-uid")
	object.SetResourceVersion("rv-1")

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient}
	session := &fakeObservationSession{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).EnableMigrationBackfill()

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	require.Len(t, session.backfills, 1)
	assert.Equal(t, "0b6fe901-7b52-4a78-9cbf-e398728ef67e", session.backfills[0].IdentityID)
	assert.Equal(t, "subscriptions", session.backfills[0].Namespace)
	assert.Equal(t, "migration-sub", session.backfills[0].Name)

	require.Len(t, client.patches, 1)
	var patch []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &patch))
	require.Len(t, patch, 3)
	status, ok := patch[2]["value"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "0b6fe901-7b52-4a78-9cbf-e398728ef67e", status["subscriptionIdentityId"])
	assert.Equal(t, true, status["subscriptionIdentityMigrationReady"])
	assert.NotEmpty(t, status["subscriptionIdentityObservedAt"])
	assert.Empty(t, status["subscriptionIdentityMigrationMessage"])
}

func TestObserverRefreshResolvesAllSubscriptionsBeforeBackfill(t *testing.T) {
	first := &unstructured.Unstructured{}
	first.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	first.SetName("first-sub")
	first.SetNamespace("subscriptions-a")
	first.SetUID("first-uid")
	first.SetResourceVersion("rv-1")

	second := &unstructured.Unstructured{}
	second.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	second.SetName("second-sub")
	second.SetNamespace("subscriptions-a")
	second.SetUID("second-uid")
	second.SetResourceVersion("rv-2")

	gvr := subscription.GVR()
	client := &recordingDynamicClient{Interface: fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		first, second,
	)}
	session := &fakeObservationSession{}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions-a",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).EnableMigrationBackfill()

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	require.Len(t, session.backfills, 2)
	require.Len(t, session.calls, 4)
	lastResolveIndex := 0
	for index, call := range session.calls {
		if strings.HasPrefix(call, "resolve:") {
			lastResolveIndex = index
		}
	}
	for _, call := range session.calls[:lastResolveIndex] {
		assert.False(t, strings.HasPrefix(call, "backfill:"), "backfill ran before all subscriptions were resolved: %s", call)
	}
}

func TestObserverRefreshReportsBlockedMigration(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("blocked-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("blocked-uid")
	object.SetResourceVersion("rv-1")

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient}
	session := &fakeObservationSession{backfillErr: errors.New("ambiguous subscription name")}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: session},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	).EnableMigrationBackfill()

	observer.Refresh(context.Background())

	assert.True(t, session.committed)
	require.Len(t, client.patches, 1)
	var patch []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &patch))
	require.Len(t, patch, 3)
	status, ok := patch[2]["value"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, status["subscriptionIdentityMigrationReady"])
	assert.NotEmpty(t, status["subscriptionIdentityObservedAt"])
	assert.Equal(t, "ambiguous subscription name", status["subscriptionIdentityMigrationMessage"])
}

func TestObserverRefreshPreservesExistingStatus(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("existing-status-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("existing-status-uid")
	object.SetResourceVersion("rv-1")
	require.NoError(t, unstructured.SetNestedField(object.Object, "Ready", "status", "phase"))

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: &fakeObservationSession{}},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	)

	observer.Refresh(context.Background())

	require.Len(t, client.patches, 1)
	var patch []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &patch))
	require.Len(t, patch, 3)
	assert.Equal(t, "/metadata/resourceVersion", patch[1]["path"])
	assert.Equal(t, "add", patch[2]["op"])
	assert.Equal(t, "/status/subscriptionIdentityId", patch[2]["path"])
}

func TestObserverStatusPatchRetriesConflict(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("conflict-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("conflict-uid")
	object.SetResourceVersion("rv-1")
	require.NoError(t, unstructured.SetNestedField(object.Object, "listed-identity", "status", "subscriptionIdentityId"))

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient, conflictFirstPatch: true}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: &fakeObservationSession{}},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	)

	require.NoError(t, observer.ApplyStatus(context.Background(), subscriptionidentity.SubscriptionObject{
		Namespace:              "subscriptions",
		Name:                   "conflict-sub",
		ObjectUID:              "conflict-uid",
		ResourceVersion:        "rv-1",
		ListedStatusIdentityID: "listed-identity",
		StatusIdentityID:       "0b6fe901-7b52-4a78-9cbf-e398728ef67e",
		StatusPresent:          true,
	}))

	require.Len(t, client.patches, 2)
	var first, second []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &first))
	require.NoError(t, json.Unmarshal(client.patches[1], &second))
	assert.Equal(t, "rv-1", first[1]["value"])
	assert.Equal(t, "rv-2", second[1]["value"])
	assert.Equal(t, "0b6fe901-7b52-4a78-9cbf-e398728ef67e", second[2]["value"])
}

func TestObserverStatusPatchRejectsUnexpectedIdentityAfterConflict(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("unexpected-identity-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("unexpected-identity-uid")
	object.SetResourceVersion("rv-1")
	require.NoError(t, unstructured.SetNestedField(object.Object, "listed-identity", "status", "subscriptionIdentityId"))

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{
		Interface:          baseClient,
		conflictFirstPatch: true,
		conflictIdentityID: "unexpected-identity",
	}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: &fakeObservationSession{}},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	)

	err := observer.ApplyStatus(context.Background(), subscriptionidentity.SubscriptionObject{
		Namespace:              "subscriptions",
		Name:                   "unexpected-identity-sub",
		ObjectUID:              "unexpected-identity-uid",
		ResourceVersion:        "rv-1",
		ListedStatusIdentityID: "listed-identity",
		StatusIdentityID:       "0b6fe901-7b52-4a78-9cbf-e398728ef67e",
		StatusPresent:          true,
	})

	require.ErrorIs(t, err, api_keys.ErrSubscriptionIdentityMismatch)
	require.Len(t, client.patches, 1)
}

func TestObserverStatusPatchRetriesStaleResourceVersion(t *testing.T) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
	})
	object.SetName("stale-sub")
	object.SetNamespace("subscriptions")
	object.SetUID("stale-uid")
	object.SetResourceVersion("rv-1")

	gvr := subscription.GVR()
	baseClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "MaaSSubscriptionList"},
		object,
	)
	client := &recordingDynamicClient{Interface: baseClient, invalidFirstPatch: true}
	observer := subscriptionidentity.NewObserver(
		&fakeIdentityStore{session: &fakeObservationSession{}},
		client,
		"subscriptions",
		"tenant-a",
		"11111111-2222-3333-4444-555555555555",
		time.Hour,
		30*time.Second,
		logger.Development(),
	)

	require.NoError(t, observer.ApplyStatus(context.Background(), subscriptionidentity.SubscriptionObject{
		Namespace:        "subscriptions",
		Name:             "stale-sub",
		ObjectUID:        "stale-uid",
		ResourceVersion:  "rv-1",
		StatusIdentityID: "0b6fe901-7b52-4a78-9cbf-e398728ef67e",
	}))

	require.Len(t, client.patches, 2)
	var second []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[1], &second))
	assert.Equal(t, "rv-2", second[1]["value"])
}
