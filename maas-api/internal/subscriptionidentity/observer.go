package subscriptionidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// SubscriptionObject is the minimal authoritative Kubernetes observation needed
// to resolve a subscription incarnation.
type SubscriptionObject struct {
	Namespace        string
	Name             string
	ObjectUID        string
	ResourceVersion  string
	StatusIdentityID string
	StatusPresent    bool
}

type Observer struct {
	store      api_keys.SubscriptionIdentityStore
	client     dynamic.Interface
	namespace  string
	tenant     string
	observerID string
	interval   time.Duration
	settle     time.Duration
	logger     *logger.Logger
}

func NewObserver(
	store api_keys.SubscriptionIdentityStore,
	client dynamic.Interface,
	namespace string,
	tenant string,
	observerID string,
	interval time.Duration,
	settle time.Duration,
	log *logger.Logger,
) *Observer {
	if log == nil {
		log = logger.Production()
	}
	return &Observer{
		store:      store,
		client:     client,
		namespace:  namespace,
		tenant:     tenant,
		observerID: observerID,
		interval:   interval,
		settle:     settle,
		logger:     log,
	}
}

func (observer *Observer) Run(ctx context.Context) {
	observer.Refresh(ctx)

	ticker := time.NewTicker(observer.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observer.Refresh(ctx)
		}
	}
}

func (observer *Observer) Refresh(ctx context.Context) {
	session, err := observer.store.BeginSubscriptionObservation(ctx, observer.observerID, observer.tenantName())
	if err != nil {
		observer.logger.Error("Failed to begin subscription observation", "error", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = session.Rollback()
		}
	}()

	current, err := session.CurrentObservations(ctx)
	if err != nil {
		observer.logger.Error("Failed to read current subscription observations", "error", err)
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	objects, err := observer.list(listCtx)
	cancel()
	if err != nil {
		observer.logger.Error("Failed to list MaaS subscriptions from Kubernetes API", "error", err)
		return
	}
	if err := session.Heartbeat(ctx); err != nil {
		observer.logger.Error("Failed to refresh subscription observer heartbeat", "error", err)
		return
	}

	present := make(map[string]struct{}, len(objects))
	statusWrites := make([]SubscriptionObject, 0, len(objects))
	for _, object := range objects {
		present[object.Namespace+"/"+object.Name] = struct{}{}
		identityID, resolveErr := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
			Namespace:        object.Namespace,
			Name:             object.Name,
			ObjectUID:        object.ObjectUID,
			StatusIdentityID: object.StatusIdentityID,
			Settle:           observer.settle,
		})
		if resolveErr != nil {
			observer.logger.Warn("Subscription identity remains pending",
				"subscription", object.Namespace+"/"+object.Name,
				"error", resolveErr,
			)
			continue
		}
		if object.StatusIdentityID != identityID {
			object.StatusIdentityID = identityID
			statusWrites = append(statusWrites, object)
		}
	}

	for _, observation := range current {
		if _, found := present[observation.Namespace+"/"+observation.Name]; found {
			continue
		}
		if err := session.MarkAbsent(ctx, observation.SubscriptionID); err != nil {
			observer.logger.Error("Failed to mark subscription observation absent",
				"subscription", observation.Namespace+"/"+observation.Name,
				"error", err,
			)
			return
		}
	}

	if err := session.Commit(); err != nil {
		observer.logger.Error("Failed to commit subscription observations", "error", err)
		return
	}
	committed = true

	for _, object := range statusWrites {
		if err := observer.ApplyStatus(ctx, object); err != nil {
			observer.logger.Error("Failed to write subscription identity status",
				"subscription", object.Namespace+"/"+object.Name,
				"error", err,
			)
		}
	}
}

func (observer *Observer) tenantName() string {
	return observer.tenant
}

func (observer *Observer) list(ctx context.Context) ([]SubscriptionObject, error) {
	list, err := observer.client.Resource(subscription.GVR()).Namespace(observer.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list MaaSSubscriptions: %w", err)
	}

	objects := make([]SubscriptionObject, 0, len(list.Items))
	for index := range list.Items {
		item := &list.Items[index]
		if item.GetDeletionTimestamp() != nil {
			continue
		}
		statusIdentityID, _, _ := unstructured.NestedString(item.Object, "status", "subscriptionIdentityId")
		_, statusPresent := item.Object["status"].(map[string]any)
		objects = append(objects, SubscriptionObject{
			Namespace:        item.GetNamespace(),
			Name:             item.GetName(),
			ObjectUID:        string(item.GetUID()),
			ResourceVersion:  item.GetResourceVersion(),
			StatusIdentityID: statusIdentityID,
			StatusPresent:    statusPresent,
		})
	}
	return objects, nil
}

func (observer *Observer) ApplyStatus(ctx context.Context, object SubscriptionObject) error {
	if err := observer.patchStatus(ctx, object); err == nil {
		return nil
	} else if !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
		return err
	}

	current, err := observer.client.Resource(subscription.GVR()).Namespace(object.Namespace).Get(ctx, object.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to re-read subscription for identity status conflict: %w", err)
	}
	if string(current.GetUID()) != object.ObjectUID {
		return fmt.Errorf("%w: subscription object was replaced during identity status write", api_keys.ErrSubscriptionIdentityMismatch)
	}
	currentIdentityID, _, _ := unstructured.NestedString(current.Object, "status", "subscriptionIdentityId")
	if currentIdentityID != "" && currentIdentityID != object.StatusIdentityID {
		return fmt.Errorf("%w: subscription identity changed during status write", api_keys.ErrSubscriptionIdentityMismatch)
	}
	_, statusPresent := current.Object["status"].(map[string]any)
	object.ResourceVersion = current.GetResourceVersion()
	object.StatusPresent = statusPresent
	if err := observer.patchStatus(ctx, object); err != nil {
		return fmt.Errorf("failed to retry UID-guarded subscription identity status: %w", err)
	}
	return nil
}

func (observer *Observer) patchStatus(ctx context.Context, object SubscriptionObject) error {
	patch := []map[string]any{
		{
			"op":    "test",
			"path":  "/metadata/uid",
			"value": object.ObjectUID,
		},
		{
			"op":    "test",
			"path":  "/metadata/resourceVersion",
			"value": object.ResourceVersion,
		},
	}
	if object.StatusPresent {
		patch = append(patch, map[string]any{
			"op":    "add",
			"path":  "/status/subscriptionIdentityId",
			"value": object.StatusIdentityID,
		})
	} else {
		patch = append(patch, map[string]any{
			"op":   "add",
			"path": "/status",
			"value": map[string]any{
				"subscriptionIdentityId": object.StatusIdentityID,
			},
		})
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to encode status patch: %w", err)
	}
	if _, err := observer.client.Resource(subscription.GVR()).Namespace(object.Namespace).Patch(
		ctx, object.Name, types.JSONPatchType, data, metav1.PatchOptions{}, "status",
	); err != nil {
		return fmt.Errorf("failed to apply UID-guarded subscription identity status: %w", err)
	}
	return nil
}
