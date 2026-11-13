package subscriptionidentity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

type Handler struct {
	store               api_keys.SubscriptionIdentityStore
	client              dynamic.Interface
	namespace           string
	tenant              string
	observerID          string
	settle              time.Duration
	maxObservationAge   time.Duration
	logger              *logger.Logger
	verificationTimeout time.Duration
}

type absenceRequest struct {
	Namespace              string `json:"namespace"`
	Name                   string `json:"name"`
	ObjectUID              string `json:"objectUID"`
	SubscriptionIdentityID string `json:"subscriptionIdentityId"`
	RetainAPIKeys          bool   `json:"retainAPIKeys"`
}

type absenceResponse struct {
	Result         string `json:"result"`
	RevokedAPIKeys int    `json:"revokedAPIKeys"`
}

func NewHandler(
	log *logger.Logger,
	store api_keys.SubscriptionIdentityStore,
	client dynamic.Interface,
	namespace, tenant, observerID string,
	settle, maxObservationAge time.Duration,
) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		store:               store,
		client:              client,
		namespace:           namespace,
		tenant:              tenant,
		observerID:          observerID,
		settle:              settle,
		maxObservationAge:   maxObservationAge,
		logger:              log,
		verificationTimeout: 10 * time.Second,
	}
}

func (handler *Handler) RecordAbsence(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")

	var request absenceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid absence request"})
		return
	}
	if namespace == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subscription namespace and name are required"})
		return
	}
	if namespace != handler.namespace {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace is not served by this observer"})
		return
	}
	if request.Namespace != "" && request.Namespace != namespace {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace does not match route"})
		return
	}
	if request.Name != "" && request.Name != name {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name does not match route"})
		return
	}
	if request.ObjectUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "objectUID is required"})
		return
	}

	object, err := handler.getDeletingSubscription(c.Request.Context(), namespace, name, request.ObjectUID)
	if err != nil {
		handler.logger.Error("Failed to verify deleting subscription", "subscription", namespace+"/"+name, "error", err)
		c.JSON(http.StatusConflict, gin.H{"error": "subscription identity verification failed"})
		return
	}

	statusIdentityID, found, err := unstructured.NestedString(object.Object, "status", "subscriptionIdentityId")
	if err != nil {
		handler.logger.Error("Failed to read subscription identity status", "subscription", namespace+"/"+name, "error", err)
		c.JSON(http.StatusConflict, gin.H{"error": "subscription identity verification failed"})
		return
	}
	if found && request.SubscriptionIdentityID == "" {
		request.SubscriptionIdentityID = statusIdentityID
	}
	if found && request.SubscriptionIdentityID != statusIdentityID {
		c.JSON(http.StatusConflict, gin.H{"error": "subscription identity does not match Kubernetes status"})
		return
	}

	session, err := handler.store.BeginSubscriptionObservation(c.Request.Context(), handler.observerID, handler.tenant)
	if err != nil {
		handler.logger.Error("Failed to begin subscription absence transaction", "subscription", namespace+"/"+name, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record subscription absence"})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = session.Rollback()
		}
	}()

	result, revokedKeys, err := session.RecordAbsenceAndCleanup(
		c.Request.Context(),
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              namespace,
			Name:                   name,
			ObjectUID:              request.ObjectUID,
			SubscriptionIdentityID: request.SubscriptionIdentityID,
			RetainAPIKeys:          request.RetainAPIKeys,
		},
		handler.settle,
		handler.maxObservationAge,
	)
	if err != nil {
		handler.logger.Error("Failed to record subscription absence", "subscription", namespace+"/"+name, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record subscription absence"})
		return
	}
	if err := session.Commit(); err != nil {
		handler.logger.Error("Failed to commit subscription absence", "subscription", namespace+"/"+name, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record subscription absence"})
		return
	}
	committed = true

	status := http.StatusConflict
	switch result {
	case api_keys.SubscriptionCleanupBlockedEmpty,
		api_keys.SubscriptionCleanupBlockedStale,
		api_keys.SubscriptionCleanupBlockedBarrier,
		api_keys.SubscriptionCleanupBlockedUnverified,
		api_keys.SubscriptionCleanupNotFound:
		status = http.StatusConflict
	case api_keys.SubscriptionCleanupRetained,
		api_keys.SubscriptionCleanupRetainedPolicy,
		api_keys.SubscriptionCleanupCleaned:
		status = http.StatusOK
	}
	c.JSON(status, absenceResponse{Result: string(result), RevokedAPIKeys: revokedKeys})
}

func (handler *Handler) getDeletingSubscription(ctx context.Context, namespace, name, objectUID string) (*unstructured.Unstructured, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, handler.verificationTimeout)
	defer cancel()

	list, err := handler.client.Resource(subscription.GVR()).Namespace(namespace).List(verifyCtx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list MaaSSubscriptions: %w", err)
	}
	for index := range list.Items {
		item := &list.Items[index]
		if item.GetNamespace() != namespace || item.GetName() != name {
			continue
		}
		if string(item.GetUID()) != objectUID {
			return nil, errors.New("object UID does not match deleting subscription")
		}
		if item.GetDeletionTimestamp() == nil {
			return nil, errors.New("subscription is not deleting")
		}
		return item, nil
	}
	return nil, errors.New("deleting subscription was not found")
}
