package subscriptionidentity

// The observer is the Kubernetes-side source of truth for subscription
// presence. Refresh intentionally holds the database observation transaction
// across an uncached Kubernetes list: current observations, the fresh list, and
// peer barriers must describe the same observation cycle. Status writes happen
// after commit and are UID/resource-version guarded so a replacement object
// cannot inherit the identity of the incarnation that was observed.

import (
	"context"
	"encoding/json"
	stderrors "errors"
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

	MigrationReady      *bool
	MigrationMessage    string
	MigrationObservedAt time.Time
}

type Observer struct {
	store               api_keys.SubscriptionIdentityStore
	client              dynamic.Interface
	namespace           string
	tenant              string
	observerID          string
	interval            time.Duration
	settle              time.Duration
	rejoinFreshness     time.Duration
	retirementFreshness time.Duration
	staleAfter          time.Duration
	retireStale         bool
	backfill            bool
	metrics             MetricsRecorder
	logger              *logger.Logger
}

type MetricsRecorder interface {
	RecordSubscriptionObserverRetirement(tenant, result string, revokedAPIKeys int)
	RecordSubscriptionObserverRejoin(tenant, result string)
}

type resolvedSubscription struct {
	object     SubscriptionObject
	identityID string
}

type resolvedObservations struct {
	present                    map[string]struct{}
	statusWrites               []SubscriptionObject
	resolved                   []resolvedSubscription
	migrationBlocked           bool
	resolutionFailed           bool
	resolutionPendingOnly      bool
	retirementResolutionFailed bool
	observedAt                 time.Time
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

// EnableMigrationBackfill makes the observer bind legacy API keys before it
// reports migration readiness.
func (observer *Observer) EnableMigrationBackfill() *Observer {
	observer.backfill = true
	return observer
}

// EnableObserverRejoin permits a retired observer to rejoin only after a
// complete, transactionally refreshed observation cycle and with a fresh peer.
func (observer *Observer) EnableObserverRejoin(freshWithin time.Duration) *Observer {
	observer.rejoinFreshness = freshWithin
	return observer
}

// EnableStaleObserverRetirement lets a fresh observer retire peers that stopped
// heartbeating after a cluster leaves the ingress set. The threshold is the
// safety barrier for identities which only that stale observer observed.
func (observer *Observer) EnableStaleObserverRetirement(freshWithin, staleAfter time.Duration) *Observer {
	observer.retirementFreshness = freshWithin
	observer.staleAfter = staleAfter
	observer.retireStale = true
	return observer
}

func (observer *Observer) WithMetricsRecorder(recorder MetricsRecorder) *Observer {
	observer.metrics = recorder
	return observer
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
	rejoinPending := false
	rejoinMetricRecorded := false
	recordRejoinOutcome := func(result string) {
		if !rejoinMetricRecorded {
			observer.recordRejoin(result)
			rejoinMetricRecorded = true
		}
	}
	defer func() {
		if rejoinPending && !committed && !rejoinMetricRecorded {
			recordRejoinOutcome("failure")
		}
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
	heartbeatErr := session.Heartbeat(ctx)
	rejoinPending = stderrors.Is(heartbeatErr, api_keys.ErrObserverRetired)
	if heartbeatErr != nil && !rejoinPending {
		observer.logger.Error("Failed to refresh subscription observer heartbeat", "error", heartbeatErr)
		return
	}
	if rejoinPending && observer.rejoinFreshness <= 0 {
		recordRejoinOutcome("blocked")
		observer.logger.Error("Retired subscription observer cannot rejoin because observer rejoin is not configured")
		return
	}

	refreshed := observer.resolvePresentSubscriptions(ctx, session, objects)

	for _, observation := range current {
		if _, found := refreshed.present[observation.Namespace+"/"+observation.Name]; found {
			continue
		}
		if err := session.MarkAbsent(ctx, observation.SubscriptionID, false); err != nil {
			observer.logger.Error("Failed to mark subscription observation absent",
				"subscription", observation.Namespace+"/"+observation.Name,
				"error", err,
			)
			return
		}
	}

	if refreshed.migrationBlocked {
		observer.logger.Warn("Subscription identity migration is blocked for one or more subscriptions")
	}

	if rejoinPending && refreshed.resolutionPendingOnly {
		if err := session.Commit(); err != nil {
			observer.logger.Error("Failed to commit pending subscription identity rotation", "error", err)
			return
		}
		committed = true
		recordRejoinOutcome("blocked")
		observer.logger.Warn("Retired subscription observer rejoin blocked until pending identity rotation settles")
		return
	}

	if rejoinPending {
		if !observer.reactivate(ctx, session, refreshed.resolutionFailed, recordRejoinOutcome) {
			return
		}
	}

	var retirements []api_keys.SubscriptionObserverRetirement
	if observer.retireStale {
		var ok bool
		retirements, ok = observer.retireStaleObservers(ctx, session, refreshed.retirementResolutionFailed)
		if !ok {
			return
		}
	}

	if err := session.Commit(); err != nil {
		observer.logger.Error("Failed to commit subscription observations", "error", err)
		if observer.retireStale {
			observer.recordRetirement("failure", 0)
		}
		return
	}
	committed = true

	if rejoinPending {
		recordRejoinOutcome("success")
		observer.logger.Info("Retired subscription observer safely rejoined")
	}
	for _, retirement := range retirements {
		observer.recordRetirement(string(retirement.Result), retirement.RevokedAPIKeys)
		observer.logger.Warn("Retired stale subscription observer",
			"observerID", retirement.ObserverID,
			"result", retirement.Result,
			"revokedAPIKeys", retirement.RevokedAPIKeys,
		)
	}

	for _, object := range refreshed.statusWrites {
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

func (observer *Observer) recordRetirement(result string, revokedAPIKeys int) {
	if observer.metrics != nil {
		observer.metrics.RecordSubscriptionObserverRetirement(observer.tenant, result, revokedAPIKeys)
	}
}

func (observer *Observer) recordRejoin(result string) {
	if observer.metrics != nil {
		observer.metrics.RecordSubscriptionObserverRejoin(observer.tenant, result)
	}
}

func (observer *Observer) resolvePresentSubscriptions(
	ctx context.Context,
	session api_keys.SubscriptionObservationSession,
	objects []SubscriptionObject,
) resolvedObservations {
	refreshed := resolvedObservations{
		present:      make(map[string]struct{}, len(objects)),
		statusWrites: make([]SubscriptionObject, 0, len(objects)),
		resolved:     make([]resolvedSubscription, 0, len(objects)),
		observedAt:   time.Now().UTC(),
	}

	for _, object := range objects {
		refreshed.present[object.Namespace+"/"+object.Name] = struct{}{}
		identityID, resolveErr := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
			Namespace:        object.Namespace,
			Name:             object.Name,
			ObjectUID:        object.ObjectUID,
			StatusIdentityID: object.StatusIdentityID,
			Settle:           observer.settle,
		})
		if resolveErr != nil {
			if stderrors.Is(resolveErr, api_keys.ErrSubscriptionIdentityPending) {
				refreshed.resolutionPendingOnly = true
				observer.logger.Warn("Subscription identity remains pending",
					"subscription", object.Namespace+"/"+object.Name,
					"error", resolveErr,
				)
			} else {
				refreshed.resolutionFailed = true
				refreshed.resolutionPendingOnly = false
				refreshed.retirementResolutionFailed = true
				observer.logger.Error("Subscription identity resolution failed",
					"subscription", object.Namespace+"/"+object.Name,
					"error", resolveErr,
				)
			}
			if observer.backfill {
				ready := false
				object.MigrationReady = &ready
				object.MigrationMessage = truncateMigrationMessage(resolveErr.Error())
				object.MigrationObservedAt = refreshed.observedAt
				refreshed.statusWrites = append(refreshed.statusWrites, object)
				refreshed.migrationBlocked = true
			}
			continue
		}
		statusIdentityChanged := object.StatusIdentityID != identityID
		object.StatusIdentityID = identityID
		refreshed.resolved = append(refreshed.resolved, resolvedSubscription{
			object:     object,
			identityID: identityID,
		})
		if statusIdentityChanged && !observer.backfill {
			refreshed.statusWrites = append(refreshed.statusWrites, object)
		}
	}

	for _, resolved := range refreshed.resolved {
		if !observer.backfill {
			continue
		}
		object := resolved.object
		_, backfillErr := session.BackfillSubscriptionKeys(ctx, resolved.identityID, object.Namespace, object.Name)
		ready := backfillErr == nil
		object.MigrationReady = &ready
		object.MigrationObservedAt = refreshed.observedAt
		if backfillErr != nil {
			object.MigrationMessage = truncateMigrationMessage(backfillErr.Error())
			refreshed.migrationBlocked = true
			observer.logger.Warn("Subscription identity key backfill blocked",
				"subscription", object.Namespace+"/"+object.Name,
				"error", backfillErr,
			)
		} else {
			object.MigrationMessage = ""
		}
		refreshed.statusWrites = append(refreshed.statusWrites, object)
	}
	return refreshed
}

func (observer *Observer) reactivate(
	ctx context.Context,
	session api_keys.SubscriptionObservationSession,
	resolutionFailed bool,
	recordRejoin func(string),
) bool {
	if resolutionFailed {
		recordRejoin("blocked")
		observer.logger.Error("Retired subscription observer rejoin blocked because identity resolution failed")
		return false
	}
	if err := session.ReactivateRetiredObserver(ctx, observer.rejoinFreshness); err != nil {
		result := "failure"
		if stderrors.Is(err, api_keys.ErrObserverRejoinBlocked) {
			result = "blocked"
		}
		recordRejoin(result)
		observer.logger.Error("Failed to reactivate retired subscription observer", "error", err)
		return false
	}
	return true
}

func (observer *Observer) retireStaleObservers(
	ctx context.Context,
	session api_keys.SubscriptionObservationSession,
	retirementResolutionFailed bool,
) ([]api_keys.SubscriptionObserverRetirement, bool) {
	if retirementResolutionFailed {
		observer.recordRetirement("blocked", 0)
		observer.logger.Warn("Stale subscription observer retirement skipped because identity resolution failed")
		return nil, true
	}
	retirements, err := session.RetireStaleObservers(ctx, observer.retirementFreshness, observer.staleAfter, observer.settle)
	if err != nil {
		observer.recordRetirement("failure", 0)
		observer.logger.Error("Failed to retire stale subscription observers", "error", err)
		return nil, false
	}
	return retirements, true
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
		if object.MigrationReady != nil {
			patch = append(patch,
				map[string]any{
					"op":    "add",
					"path":  "/status/subscriptionIdentityMigrationReady",
					"value": *object.MigrationReady,
				},
				map[string]any{
					"op":    "add",
					"path":  "/status/subscriptionIdentityObservedAt",
					"value": object.MigrationObservedAt.UTC().Format(time.RFC3339),
				},
				map[string]any{
					"op":    "add",
					"path":  "/status/subscriptionIdentityMigrationMessage",
					"value": object.MigrationMessage,
				},
			)
		}
	} else {
		status := map[string]any{
			"subscriptionIdentityId": object.StatusIdentityID,
		}
		if object.MigrationReady != nil {
			status["subscriptionIdentityMigrationReady"] = *object.MigrationReady
			status["subscriptionIdentityObservedAt"] = object.MigrationObservedAt.UTC().Format(time.RFC3339)
			status["subscriptionIdentityMigrationMessage"] = object.MigrationMessage
		}
		patch = append(patch, map[string]any{
			"op":    "add",
			"path":  "/status",
			"value": status,
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

func truncateMigrationMessage(message string) string {
	if len(message) <= 1024 {
		return message
	}
	return message[:1024]
}
