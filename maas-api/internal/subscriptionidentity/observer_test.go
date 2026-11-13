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
	observations []api_keys.SubscriptionObservation
	heartbeat    bool
	resolved     []api_keys.ResolveSubscriptionIdentityRequest
	absent       []string
	absences     []api_keys.SubscriptionAbsenceRequest
	committed    bool
	rolledBack   bool
	result       api_keys.SubscriptionCleanupResult
}

func (session *fakeObservationSession) CurrentObservations(context.Context) ([]api_keys.SubscriptionObservation, error) {
	return session.observations, nil
}

func (session *fakeObservationSession) Heartbeat(context.Context) error {
	session.heartbeat = true
	return nil
}

func (session *fakeObservationSession) ResolveSubscriptionIdentity(_ context.Context, request api_keys.ResolveSubscriptionIdentityRequest) (string, error) {
	session.resolved = append(session.resolved, request)
	return "0b6fe901-7b52-4a78-9cbf-e398728ef67e", nil
}

func (session *fakeObservationSession) MarkAbsent(_ context.Context, subscriptionID string) error {
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

type recordingDynamicClient struct {
	dynamic.Interface

	patchTypes         []types.PatchType
	patches            [][]byte
	subresources       [][]string
	conflictFirstPatch bool
	invalidFirstPatch  bool
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
		Namespace:        "subscriptions",
		Name:             "conflict-sub",
		ObjectUID:        "conflict-uid",
		ResourceVersion:  "rv-1",
		StatusIdentityID: "0b6fe901-7b52-4a78-9cbf-e398728ef67e",
	}))

	require.Len(t, client.patches, 2)
	var first, second []map[string]any
	require.NoError(t, json.Unmarshal(client.patches[0], &first))
	require.NoError(t, json.Unmarshal(client.patches[1], &second))
	assert.Equal(t, "rv-1", first[1]["value"])
	assert.Equal(t, "rv-2", second[1]["value"])
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
