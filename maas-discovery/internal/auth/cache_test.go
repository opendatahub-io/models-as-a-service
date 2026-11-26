package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/auth"
)

// countingClient wraps a fake clientset and counts every TokenReview and
// SubjectAccessReview the API server would receive. Cache-effectiveness
// assertions are made against these counters — never against cache internals —
// so a cache that silently never hits (the maas-api Authorino defect, PR
// #1899) fails these tests.
type countingClient struct {
	*kubefake.Clientset

	mu               sync.Mutex
	tokenReviewCalls int
	sarCalls         int
}

func newCountingClient(groups []string, authenticated, sarAllowed, adminAllowed bool) *countingClient {
	c := &countingClient{Clientset: kubefake.NewSimpleClientset()}
	c.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.tokenReviewCalls++
		c.mu.Unlock()
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: authenticated,
				User:          authv1.UserInfo{Username: "alice", Groups: groups},
			},
		}, nil
	})
	c.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.sarCalls++
		c.mu.Unlock()
		allowed := sarAllowed
		if sar, ok := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview); ok &&
			sar.Spec.ResourceAttributes != nil && sar.Spec.ResourceAttributes.Resource == "aitenants" {
			allowed = adminAllowed
		}
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	return c
}

func (c *countingClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenReviewCalls, c.sarCalls
}

// fakeClock controls time for TTL and token-expiry tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// cachedRouter builds a router whose middleware reviews through a ReviewCache
// wrapping a TokenReviewer backed by the counting client.
// cachedRouter wires the middleware through a ReviewCache with a fixed 2s
// negative TTL (tests advance the fake clock past it to assert expiry).
func cachedRouter(client *countingClient, clk auth.Clock, ttl time.Duration, maxEntries int) (*gin.Engine, *auth.ReviewCache) {
	reviewer := auth.NewTokenReviewer(slog.Default(), client, "ai-tenants")
	cache := auth.NewReviewCache(reviewer, auth.ReviewCacheOptions{
		TTL:         ttl,
		NegativeTTL: 2 * time.Second,
		MaxEntries:  maxEntries,
		Clock:       clk,
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v1/tenants", auth.TokenReviewMiddleware(slog.Default(), cache), func(c *gin.Context) {
		username, _ := c.Get(auth.ContextKeyUsername)
		isAdmin, _ := c.Get(auth.ContextKeyIsAdmin)
		c.JSON(http.StatusOK, gin.H{"username": username, "isAdmin": isAdmin})
	})
	return r, cache
}

func doRequest(r *gin.Engine, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	return w
}

// jwtWithExpiry builds an unsigned JWT whose payload carries the given exp
// (unix seconds), for token-expiry capping tests.
func jwtWithExpiry(t *testing.T, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp})
	require.NoError(t, err)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestReviewCache_RepeatedRequestsReviewOnce is the core effectiveness guard:
// repeated requests with the same bearer token must not generate additional
// TokenReview/SAR calls within the cache window.
func TestReviewCache_RepeatedRequestsReviewOnce(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, cache := cachedRouter(client, newFakeClock(), time.Minute, 8192)

	for range 5 {
		w := doRequest(r, "token-a")
		require.Equal(t, http.StatusOK, w.Code)
	}

	tokenReviews, sars := client.counts()
	assert.Equal(t, 1, tokenReviews, "5 identical requests must make exactly 1 TokenReview call")
	assert.Equal(t, 2, sars, "5 identical requests must make exactly 2 SAR calls")

	stats := cache.Stats()
	assert.Equal(t, uint64(4), stats.Hits)
	assert.Equal(t, uint64(1), stats.Misses)
	assert.Equal(t, uint64(0), stats.Evictions)
}

// TestReviewCache_DifferentTokensReviewSeparately: distinct tokens are
// distinct cache entries.
func TestReviewCache_DifferentTokensReviewSeparately(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, _ := cachedRouter(client, newFakeClock(), time.Minute, 8192)

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)
	require.Equal(t, http.StatusOK, doRequest(r, "token-b").Code)

	tokenReviews, sars := client.counts()
	assert.Equal(t, 2, tokenReviews)
	assert.Equal(t, 4, sars)
}

// TestReviewCache_KeyStability: request-varying data (path, other headers)
// must not split cache entries — the maas-api cache-key splitting defect.
func TestReviewCache_KeyStability(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, _ := cachedRouter(client, newFakeClock(), time.Minute, 8192)

	for range 3 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
		req.Header.Set("Authorization", "Bearer token-a")
		req.Header.Set("X-Request-Id", "different-every-time")
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}

	tokenReviews, _ := client.counts()
	assert.Equal(t, 1, tokenReviews, "request-varying headers must not split the cache entry")
}

// TestReviewCache_TTLExpiry: after the TTL passes, the token is re-reviewed.
func TestReviewCache_TTLExpiry(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	clk := newFakeClock()
	r, _ := cachedRouter(client, clk, 30*time.Second, 8192)

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)
	clk.advance(29 * time.Second)
	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)
	clk.advance(2 * time.Second)
	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)

	tokenReviews, _ := client.counts()
	assert.Equal(t, 2, tokenReviews, "entry must expire after the TTL")
}

// TestReviewCache_TokenExpiryCapsTTL: a cached allow is never used beyond the
// token's own validity, even when the configured TTL is longer.
func TestReviewCache_TokenExpiryCapsTTL(t *testing.T) {
	clk := newFakeClock()
	token := jwtWithExpiry(t, clk.Now().Add(10*time.Second).Unix())

	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, _ := cachedRouter(client, clk, time.Hour, 8192)

	require.Equal(t, http.StatusOK, doRequest(r, token).Code)
	clk.advance(9 * time.Second)
	require.Equal(t, http.StatusOK, doRequest(r, token).Code)
	// Past the token's exp: the cache entry must have expired already.
	clk.advance(2 * time.Second)
	require.Equal(t, http.StatusOK, doRequest(r, token).Code)

	tokenReviews, _ := client.counts()
	assert.Equal(t, 2, tokenReviews, "cache entry must not outlive the token's exp claim")
}

// TestReviewCache_NegativeResultShortTTL: rejected tokens are cached for the
// shorter negative TTL only.
func TestReviewCache_NegativeResultShortTTL(t *testing.T) {
	client := newCountingClient(nil, false, false, false)
	clk := newFakeClock()
	r, _ := cachedRouter(client, clk, time.Hour, 8192)

	require.Equal(t, http.StatusUnauthorized, doRequest(r, "bad-token").Code)
	require.Equal(t, http.StatusUnauthorized, doRequest(r, "bad-token").Code)

	tokenReviews, _ := client.counts()
	assert.Equal(t, 1, tokenReviews, "rejected token must be cached for the negative TTL")

	clk.advance(3 * time.Second)
	require.Equal(t, http.StatusUnauthorized, doRequest(r, "bad-token").Code)
	tokenReviews, _ = client.counts()
	assert.Equal(t, 2, tokenReviews, "rejected token entry must expire after the negative TTL")
}

// TestReviewCache_ErrorsNotCached: API-server failures fail closed and are
// never cached, so a transient error cannot stick.
func TestReviewCache_ErrorsNotCached(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	calls := 0
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, nil, assert.AnError
	})
	reviewer := auth.NewTokenReviewer(slog.Default(), client, "ai-tenants")
	cache := auth.NewReviewCache(reviewer, auth.ReviewCacheOptions{
		TTL: time.Hour, NegativeTTL: time.Second, MaxEntries: 100,
	})

	_, err := cache.Review(context.Background(), "token-a")
	require.Error(t, err)
	_, err = cache.Review(context.Background(), "token-a")
	require.Error(t, err)

	assert.Equal(t, 2, calls, "errors must not be cached")
	stats := cache.Stats()
	assert.Equal(t, uint64(2), stats.Misses)
	assert.Equal(t, uint64(0), stats.Hits)
}

// TestReviewCache_LRUEviction: the cache is bounded; the least recently used
// entry is evicted and re-reviewed on its next request.
func TestReviewCache_LRUEviction(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, cache := cachedRouter(client, newFakeClock(), time.Minute, 2)

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)
	require.Equal(t, http.StatusOK, doRequest(r, "token-b").Code)
	require.Equal(t, http.StatusOK, doRequest(r, "token-c").Code) // evicts token-a

	tokenReviews, _ := client.counts()
	assert.Equal(t, 3, tokenReviews)
	assert.Equal(t, uint64(1), cache.Stats().Evictions)

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code) // re-reviewed
	tokenReviews, _ = client.counts()
	assert.Equal(t, 4, tokenReviews)
}

// TestReviewCache_RecentEntriesSurviveEviction: touching an entry marks it
// recently used.
func TestReviewCache_RecentEntriesSurviveEviction(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, _ := cachedRouter(client, newFakeClock(), time.Minute, 2)

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)
	require.Equal(t, http.StatusOK, doRequest(r, "token-b").Code)
	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code) // token-a now most recent
	require.Equal(t, http.StatusOK, doRequest(r, "token-c").Code) // evicts token-b

	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code) // still cached

	tokenReviews, _ := client.counts()
	assert.Equal(t, 3, tokenReviews)
}

// TestReviewCache_Disabled: TTL 0 disables caching; every request reviews.
func TestReviewCache_Disabled(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	reviewer := auth.NewTokenReviewer(slog.Default(), client, "ai-tenants")
	cache := auth.NewReviewCache(reviewer, auth.ReviewCacheOptions{TTL: 0})

	for range 3 {
		result, err := cache.Review(context.Background(), "token-a")
		require.NoError(t, err)
		assert.True(t, result.Authenticated)
	}

	tokenReviews, _ := client.counts()
	assert.Equal(t, 3, tokenReviews)
}

// TestReviewCache_InvalidOptions: misconfiguration fails fast at startup.
func TestReviewCache_InvalidOptions(t *testing.T) {
	reviewer := auth.NewTokenReviewer(slog.Default(), kubefake.NewSimpleClientset(), "ai-tenants")

	assert.Panics(t, func() { auth.NewReviewCache(nil, auth.ReviewCacheOptions{TTL: time.Minute}) })
	assert.Panics(t, func() {
		auth.NewReviewCache(reviewer, auth.ReviewCacheOptions{TTL: time.Minute, NegativeTTL: 0, MaxEntries: 10})
	})
	assert.Panics(t, func() {
		auth.NewReviewCache(reviewer, auth.ReviewCacheOptions{TTL: time.Minute, NegativeTTL: time.Second, MaxEntries: 0})
	})
}

// TestReviewCache_ConcurrentRequests: parallel requests with the same token
// must all succeed and share the cached decision once one request has
// populated it. (Coalescing concurrent cold misses is not implemented;
// deduplicating a simultaneous burst is future work.)
func TestReviewCache_ConcurrentRequests(t *testing.T) {
	client := newCountingClient([]string{"system:authenticated"}, true, true, true)
	r, _ := cachedRouter(client, newFakeClock(), time.Minute, 8192)

	// Warm the cache so the burst below reads a single shared decision.
	require.Equal(t, http.StatusOK, doRequest(r, "token-a").Code)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			w := doRequest(r, "token-a")
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
	wg.Wait()

	tokenReviews, _ := client.counts()
	assert.Equal(t, 1, tokenReviews, "concurrent requests must share one cached review")
}
