package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
)

const metricsScraperServiceAccount = "maas-metrics-scraper"

// withMetricsScraperIdentity allows the dedicated scraper at this endpoint
// without granting its token access to the Kubernetes API server's /metrics.
// Other callers continue through the usual Kubernetes authorization filter.
func withMetricsScraperIdentity(config *rest.Config, httpClient *http.Client, namespace string, handler, fallback http.Handler) (http.Handler, error) {
	client, err := authenticationclient.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create metrics TokenReview client: %w", err)
	}
	review := func(ctx context.Context, token string) (authv1.TokenReviewStatus, error) {
		result, err := client.TokenReviews().Create(ctx, &authv1.TokenReview{
			Spec: authv1.TokenReviewSpec{Token: token},
		}, metav1.CreateOptions{})
		if err != nil {
			return authv1.TokenReviewStatus{}, err
		}
		return result.Status, nil
	}
	return metricsScraperOrFallback(handler, fallback, namespace, review), nil
}

type metricsTokenReviewer func(context.Context, string) (authv1.TokenReviewStatus, error)

func metricsScraperOrFallback(handler, fallback http.Handler, namespace string, review metricsTokenReviewer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Fields(req.Header.Get("Authorization"))
		if req.Method == http.MethodGet && req.URL.Path == "/metrics" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			status, err := review(req.Context(), parts[1])
			if err == nil && status.Authenticated && status.Error == "" &&
				status.User.Username == "system:serviceaccount:"+namespace+":"+metricsScraperServiceAccount {
				handler.ServeHTTP(w, req)
				return
			}
		}
		fallback.ServeHTTP(w, req)
	})
}
