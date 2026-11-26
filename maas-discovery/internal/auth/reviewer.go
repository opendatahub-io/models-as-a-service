package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Sentinel errors returned by Reviewer implementations. The middleware maps
// them to HTTP responses; caches must not store results produced alongside an
// error (fail closed).
var (
	// ErrTokenValidation wraps TokenReview API call failures (HTTP 401).
	ErrTokenValidation = errors.New("token validation failed")
	// ErrAuthorizationCheck wraps SubjectAccessReview API call failures (HTTP 500).
	ErrAuthorizationCheck = errors.New("authorization check failed")
)

// AuthResult is the outcome of authenticating and authorizing a bearer token.
// A zero AuthResult with a nil error means the token was rejected; the
// middleware distinguishes rejection reasons through the flags below.
type AuthResult struct {
	// Authenticated is false when the TokenReview rejected the token (401).
	Authenticated bool
	// Authorized is false when the caller may not create
	// SelfSubjectAccessReviews, i.e. is not considered an authenticated
	// cluster user (403).
	Authorized bool
	Username   string
	Groups     []string
	// IsAdmin reports whether the caller may list AITenants in the AITenant
	// namespace (admin visibility for tenant discovery).
	IsAdmin bool
}

// Reviewer authenticates and authorizes a bearer token. Implementations must
// fail closed: an error must never produce a usable AuthResult.
type Reviewer interface {
	Review(ctx context.Context, token string) (AuthResult, error)
}

// TokenReviewer performs the uncached per-token API-server calls: a
// TokenReview, a SubjectAccessReview verifying the caller is an authenticated
// cluster user, and an admin-visibility SubjectAccessReview for listing
// AITenants. It is the delegate wrapped by ReviewCache.
type TokenReviewer struct {
	log               *slog.Logger
	kubeClient        kubernetes.Interface
	aitenantNamespace string
}

// NewTokenReviewer creates a Reviewer backed by live TokenReview and
// SubjectAccessReview calls. A non-empty aitenantNamespace enables the admin
// visibility check.
func NewTokenReviewer(log *slog.Logger, kubeClient kubernetes.Interface, aitenantNamespace string) *TokenReviewer {
	return &TokenReviewer{
		log:               log,
		kubeClient:        kubeClient,
		aitenantNamespace: aitenantNamespace,
	}
}

// Review validates the bearer token and the caller's visibility. API call
// failures return wrapped sentinel errors and are never cached by callers.
func (r *TokenReviewer) Review(ctx context.Context, token string) (AuthResult, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}
	result, err := r.kubeClient.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		r.log.Error("TokenReview failed", "error", err)
		return AuthResult{}, fmt.Errorf("%w: %w", ErrTokenValidation, err)
	}

	if !result.Status.Authenticated {
		r.log.Debug("token not authenticated", "error", result.Status.Error)
		return AuthResult{Authenticated: false, Authorized: false}, nil
	}

	username := result.Status.User.Username
	groups := result.Status.User.Groups

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   username,
			Groups: groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Group:    "authorization.k8s.io",
				Resource: "selfsubjectaccessreviews",
				Verb:     "create",
			},
		},
	}

	sarResult, err := r.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		r.log.Error("SubjectAccessReview failed", "error", err)
		return AuthResult{}, fmt.Errorf("%w: %w", ErrAuthorizationCheck, err)
	}

	if !sarResult.Status.Allowed {
		r.log.Debug("access denied - not an authenticated user", "reason", sarResult.Status.Reason)
		return AuthResult{Authenticated: true, Authorized: false, Username: username, Groups: groups}, nil
	}

	review := AuthResult{
		Authenticated: true,
		Authorized:    true,
		Username:      username,
		Groups:        groups,
	}

	if r.aitenantNamespace != "" {
		adminSAR := &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User:   username,
				Groups: groups,
				ResourceAttributes: &authzv1.ResourceAttributes{
					Namespace: r.aitenantNamespace,
					Verb:      "list",
					Group:     "maas.opendatahub.io",
					Resource:  "aitenants",
				},
			},
		}

		adminResult, adminErr := r.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, adminSAR, metav1.CreateOptions{})
		if adminErr != nil {
			// A failed admin check degrades to non-admin visibility; the
			// caller is still authenticated and authorized.
			r.log.Error("admin SubjectAccessReview failed", "error", adminErr)
		} else {
			review.IsAdmin = adminResult.Status.Allowed
		}
	}

	return review, nil
}
