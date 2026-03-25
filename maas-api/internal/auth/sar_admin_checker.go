package auth

import (
	"context"
	"log/slog"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// SARAdminChecker checks admin status via Kubernetes SubjectAccessReview.
// Admin is defined as: can create maasauthpolicies in the MaaS namespace.
// This aligns with RBAC from opendatahub-operator#3301 which grants admin groups
// CRUD access to MaaSAuthPolicy and MaaSSubscription resources.
type SARAdminChecker struct {
	client    kubernetes.Interface
	namespace string
}

// NewSARAdminChecker creates a SAR-based admin checker.
// The namespace parameter specifies where maasauthpolicies are checked.
func NewSARAdminChecker(client kubernetes.Interface, namespace string) *SARAdminChecker {
	if client == nil {
		panic("client cannot be nil for SARAdminChecker")
	}
	if namespace == "" {
		panic("namespace cannot be empty for SARAdminChecker")
	}
	return &SARAdminChecker{
		client:    client,
		namespace: namespace,
	}
}

// IsAdmin checks if the user can create maasauthpolicies in the configured namespace.
// This is a proxy for "is this user an admin" based on RBAC permissions.
// Returns false (fail-closed) if the check cannot be performed.
func (s *SARAdminChecker) IsAdmin(ctx context.Context, user *token.UserContext) bool {
	if s == nil || s.client == nil || user == nil || user.Username == "" {
		return false
	}

	sar := &authv1.SubjectAccessReview{
		Spec: authv1.SubjectAccessReviewSpec{
			User:   user.Username,
			Groups: user.Groups,
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: s.namespace,
				Verb:      "create",
				Group:     "maas.opendatahub.io",
				Resource:  "maasauthpolicies",
			},
		},
	}

	result, err := s.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("SAR admin check failed", "error", err.Error())
		return false
	}

	return result.Status.Allowed
}
