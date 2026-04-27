package auth_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type mockDelegate struct {
	mu       sync.Mutex
	calls    int
	response bool
	err      error
}

func (m *mockDelegate) IsAdmin(_ context.Context, _ *token.UserContext) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.response, m.err
}

func (m *mockDelegate) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	metric, ok := c.(prometheus.Metric)
	require.True(t, ok, "counter does not implement prometheus.Metric")
	var m dto.Metric
	require.NoError(t, metric.Write(&m))
	return m.GetCounter().GetValue()
}

func newTestChecker(delegate *mockDelegate) *auth.CachedAdminChecker {
	reg := prometheus.NewRegistry()
	return auth.NewCachedAdminChecker(delegate, time.Minute, 2*time.Second, 8192, reg, nil)
}

func TestCachedAdminChecker_CacheHit(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, result)

	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, result)

	assert.Equal(t, 1, delegate.getCalls(), "delegate should be called only once")
}

func TestCachedAdminChecker_CacheMiss(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "bob", Groups: []string{"users"}}

	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)
	assert.Equal(t, 1, delegate.getCalls())
}

func TestCachedAdminChecker_TTLExpiry(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, 30*time.Second, 2*time.Second, 8192, reg, fakeClock)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	_, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, 1, delegate.getCalls())

	_, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, 1, delegate.getCalls(), "should use cache before TTL")

	fakeClock.Step(31 * time.Second)

	_, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, 2, delegate.getCalls(), "should call delegate after TTL expiry")
}

func TestCachedAdminChecker_DifferentUsers(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	alice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	bob := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	_, _ = checker.IsAdmin(context.Background(), alice)
	_, _ = checker.IsAdmin(context.Background(), bob)

	assert.Equal(t, 2, delegate.getCalls(), "different users should be separate cache entries")
}

func TestCachedAdminChecker_SameUserDifferentGroups(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	adminAlice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	userAlice := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	_, _ = checker.IsAdmin(context.Background(), adminAlice)
	_, _ = checker.IsAdmin(context.Background(), userAlice)

	assert.Equal(t, 2, delegate.getCalls(), "same user with different groups should be separate cache entries")
}

func TestCachedAdminChecker_GroupOrderIrrelevant(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"b", "a", "c"}}
	user2 := &token.UserContext{Username: "alice", Groups: []string{"c", "a", "b"}}

	_, _ = checker.IsAdmin(context.Background(), user1)
	_, _ = checker.IsAdmin(context.Background(), user2)

	assert.Equal(t, 1, delegate.getCalls(), "group order should not matter for cache key")
}

func TestCachedAdminChecker_NilUserReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	result, err := checker.IsAdmin(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, result)
	assert.Equal(t, 0, delegate.getCalls())
}

func TestCachedAdminChecker_EmptyUsernameReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user := &token.UserContext{Username: "", Groups: []string{"admins"}}
	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)
	assert.Equal(t, 0, delegate.getCalls())
}

func TestCachedAdminChecker_NilCheckerReturnsFalse(t *testing.T) {
	var checker *auth.CachedAdminChecker
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCachedAdminChecker_Metrics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	checker := auth.NewCachedAdminChecker(delegate, time.Minute, 2*time.Second, 8192, reg, nil)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	_, _ = checker.IsAdmin(context.Background(), user)

	hits, misses := checker.Metrics()
	assert.InDelta(t, 0, counterValue(t, hits), 0)
	assert.InDelta(t, 1, counterValue(t, misses), 0)

	_, _ = checker.IsAdmin(context.Background(), user)

	assert.InDelta(t, 1, counterValue(t, hits), 0)
	assert.InDelta(t, 1, counterValue(t, misses), 0)
}

func TestCachedAdminChecker_ConcurrentAccess(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	var wg sync.WaitGroup
	var trueCount atomic.Int64
	errs := make(chan error, 100)

	for range 100 {
		wg.Go(func() {
			result, err := checker.IsAdmin(context.Background(), user)
			errs <- err
			if result {
				trueCount.Add(1)
			}
		})
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(100), trueCount.Load(), "all calls should return true")
}

func TestCachedAdminChecker_NilDelegatePanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(nil, time.Minute, 2*time.Second, 8192, reg, nil)
	})
}

func TestCachedAdminChecker_NonPositiveTTLPanics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(delegate, 0, 2*time.Second, 8192, reg, nil)
	})
}

func TestCachedAdminChecker_NonPositiveNegativeTTLPanics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(delegate, time.Minute, 0, 8192, reg, nil)
	})
}

func TestCachedAdminChecker_NonPositiveMaxSizePanics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(delegate, time.Minute, 2*time.Second, 0, reg, nil)
	})
}

func TestCachedAdminChecker_MaxSizeEnforced(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, time.Minute, 2*time.Second, 2, reg, fakeClock)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	user2 := &token.UserContext{Username: "bob", Groups: []string{"admins"}}
	user3 := &token.UserContext{Username: "charlie", Groups: []string{"admins"}}

	// Fill cache to max (2 entries)
	_, _ = checker.IsAdmin(context.Background(), user1)
	_, _ = checker.IsAdmin(context.Background(), user2)
	assert.Equal(t, 2, delegate.getCalls())

	// Third user: cache is full, insert is skipped but result is still returned
	result, err := checker.IsAdmin(context.Background(), user3)
	require.NoError(t, err)
	assert.True(t, result)
	assert.Equal(t, 3, delegate.getCalls())

	// user3 was not cached, so calling again should hit delegate
	_, _ = checker.IsAdmin(context.Background(), user3)
	assert.Equal(t, 4, delegate.getCalls(), "uncached entry should call delegate again")

	// user1 is still cached
	_, _ = checker.IsAdmin(context.Background(), user1)
	assert.Equal(t, 4, delegate.getCalls(), "cached entry should not call delegate")
}

func TestCachedAdminChecker_FalseResultIsCached(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)

	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)

	assert.Equal(t, 1, delegate.getCalls(), "false results should also be cached")
}

func TestCachedAdminChecker_EvictsExpiredEntries(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, 10*time.Second, 2*time.Second, 8192, reg, fakeClock)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	user2 := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	// Cache user1 at t=0
	_, err := checker.IsAdmin(context.Background(), user1)
	require.NoError(t, err)
	require.Equal(t, 1, delegate.getCalls())

	// At t=11s user1's entry is expired; calling user2 triggers eviction of user1
	fakeClock.Step(11 * time.Second)
	_, err = checker.IsAdmin(context.Background(), user2)
	require.NoError(t, err)
	require.Equal(t, 2, delegate.getCalls())

	// user1's entry was evicted, so this should call the delegate again
	_, err = checker.IsAdmin(context.Background(), user1)
	require.NoError(t, err)
	assert.Equal(t, 3, delegate.getCalls(), "evicted entry should require fresh delegate call")
}

func TestCachedAdminChecker_DelegateErrorNotCached(t *testing.T) {
	delegate := &mockDelegate{response: false, err: assert.AnError}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	result, err := checker.IsAdmin(context.Background(), user)
	require.Error(t, err)
	assert.False(t, result)
	assert.Equal(t, 1, delegate.getCalls())

	// Fix the delegate
	delegate.mu.Lock()
	delegate.response = true
	delegate.err = nil
	delegate.mu.Unlock()

	// Should call delegate again since error result was not cached
	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, result)
	assert.Equal(t, 2, delegate.getCalls())
}

func TestCachedAdminChecker_AsymmetricTTL(t *testing.T) {
	delegate := &mockDelegate{response: false}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, 30*time.Second, 2*time.Second, 8192, reg, fakeClock)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	// First call: not admin, cached with negative TTL (2s)
	result, err := checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)
	assert.Equal(t, 1, delegate.getCalls())

	// Within negative TTL: still cached
	fakeClock.Step(1 * time.Second)
	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.False(t, result)
	assert.Equal(t, 1, delegate.getCalls(), "should use cache within negative TTL")

	// After negative TTL: cache expired, call delegate again
	fakeClock.Step(2 * time.Second)

	// Change delegate to return true
	delegate.mu.Lock()
	delegate.response = true
	delegate.mu.Unlock()

	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, result)
	assert.Equal(t, 2, delegate.getCalls(), "should call delegate after negative TTL expiry")

	// Now true result cached with positive TTL (30s): verify it persists
	fakeClock.Step(10 * time.Second)
	result, err = checker.IsAdmin(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, result)
	assert.Equal(t, 2, delegate.getCalls(), "true result should be cached with long TTL")
}

func TestCachedAdminChecker_CanceledContextReturnsError(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := checker.IsAdmin(ctx, user)
	assert.False(t, result)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, delegate.getCalls(), "delegate should not be called with canceled context")
}
