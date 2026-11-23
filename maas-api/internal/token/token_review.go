package token

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TokenReviewTimeout bounds a single TokenReview API call so a slow or
// unreachable API server cannot hang request handling.
const TokenReviewTimeout = 5 * time.Second

// ErrTokenNotAuthenticated reports a token that the API server rejected or
// that does not belong to the system:authenticated group. It maps to a 401.
var ErrTokenNotAuthenticated = errors.New("token is not authenticated")

// ReviewToken validates a Kubernetes bearer token via TokenReview and returns
// the authenticated user. It is shared by the tenant discovery middleware and
// the ExtractUserInfo Kubernetes-auth fallback. Callers that need a bounded
// call should pass a context with a deadline (TokenReviewTimeout).
func ReviewToken(ctx context.Context, kubeClient kubernetes.Interface, bearerToken string) (*UserContext, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: bearerToken},
	}
	result, err := kubeClient.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("TokenReview failed: %w", err)
	}
	if !result.Status.Authenticated {
		return nil, ErrTokenNotAuthenticated
	}

	username := result.Status.User.Username
	groups := result.Status.User.Groups

	// Every authenticated Kubernetes identity carries system:authenticated;
	// require it so half-verified identities never pass.
	if !slices.Contains(groups, "system:authenticated") {
		return nil, ErrTokenNotAuthenticated
	}

	return &UserContext{
		Username:   username,
		Groups:     groups,
		AuthSource: AuthSourceK8s,
	}, nil
}
