package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
)

func TestMetricsScraperOrFallback(t *testing.T) {
	const namespace = "maas-infra"
	for _, tc := range []struct {
		name, method, path, header, username string
		authenticated, reviewError bool
		want                       int
	}{
		{"dedicated scraper", "GET", "/metrics", "Bearer token", "system:serviceaccount:maas-infra:maas-metrics-scraper", true, false, http.StatusNoContent},
		{"wrong namespace", "GET", "/metrics", "Bearer token", "system:serviceaccount:tenant:maas-metrics-scraper", true, false, http.StatusForbidden},
		{"wrong account", "GET", "/metrics", "Bearer token", "system:serviceaccount:maas-infra:other", true, false, http.StatusForbidden},
		{"invalid token", "GET", "/metrics", "Bearer token", "", false, false, http.StatusForbidden},
		{"other authorized identity", "GET", "/metrics", "Bearer legacy", "", false, false, http.StatusAccepted},
		{"review failure", "GET", "/metrics", "Bearer token", "", false, true, http.StatusForbidden},
		{"no token", "GET", "/metrics", "", "", false, false, http.StatusForbidden},
		{"wrong method", "POST", "/metrics", "Bearer token", "system:serviceaccount:maas-infra:maas-metrics-scraper", true, false, http.StatusForbidden},
		{"wrong path", "GET", "/other", "Bearer token", "system:serviceaccount:maas-infra:maas-metrics-scraper", true, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := func(_ context.Context, token string) (authv1.TokenReviewStatus, error) {
				if token != "token" && token != "legacy" {
					t.Fatalf("unexpected token review input")
				}
				if tc.reviewError {
					return authv1.TokenReviewStatus{}, errors.New("review unavailable")
				}
				return authv1.TokenReviewStatus{
					Authenticated: tc.authenticated,
					User:          authv1.UserInfo{Username: tc.username},
				}, nil
			}
			handler := metricsScraperOrFallback(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
				http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Header.Get("Authorization") == "Bearer legacy" {
						w.WriteHeader(http.StatusAccepted)
						return
					}
					w.WriteHeader(http.StatusForbidden)
				}),
				namespace, review,
			)
			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
			req.Header.Set("Authorization", tc.header)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d", resp.Code, tc.want)
			}
		})
	}
}
