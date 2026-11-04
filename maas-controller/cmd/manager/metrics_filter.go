package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const metricsScraperServiceAccount = "maas-metrics-scraper"

// metricsFilterProvider preserves the standard Kubernetes metrics authorization
// path, while allowing the dedicated scraper identity at this endpoint alone.
// The scraper has no Kubernetes API-server /metrics permission.
func metricsFilterProvider(scraperNamespace string) func(*rest.Config, *http.Client) (metricsserver.Filter, error) {
	return func(config *rest.Config, httpClient *http.Client) (metricsserver.Filter, error) {
		defaultFilter, err := filters.WithAuthenticationAndAuthorization(config, httpClient)
		if err != nil {
			return nil, err
		}
		if scraperNamespace == "" {
			return defaultFilter, nil
		}
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
		return func(log logr.Logger, handler http.Handler) (http.Handler, error) {
			fallback, err := defaultFilter(log, handler)
			if err != nil {
				return nil, err
			}
			return metricsScraperOrFallback(handler, fallback, scraperNamespace, review), nil
		}, nil
	}
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
