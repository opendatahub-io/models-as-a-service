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
}

func (m *mockDelegate) IsAdmin(_ context.Context, _ *token.UserContext) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.response
}

func (m *mockDelegate) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
}

func newTestChecker(delegate *mockDelegate) *auth.CachedAdminChecker {
	reg := prometheus.NewRegistry()
	return auth.NewCachedAdminChecker(delegate, time.Minute, reg, nil)
}

func TestCachedAdminChecker_CacheHit(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	assert.True(t, checker.IsAdmin(context.Background(), user))
	assert.True(t, checker.IsAdmin(context.Background(), user))

	assert.Equal(t, 1, delegate.getCalls(), "delegate should be called only once")
}

func TestCachedAdminChecker_CacheMiss(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "bob", Groups: []string{"users"}}

	assert.False(t, checker.IsAdmin(context.Background(), user))
	assert.Equal(t, 1, delegate.getCalls())
}

func TestCachedAdminChecker_TTLExpiry(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, 30*time.Second, reg, fakeClock)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 1, delegate.getCalls())

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 1, delegate.getCalls(), "should use cache before TTL")

	fakeClock.Step(31 * time.Second)

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 2, delegate.getCalls(), "should call delegate after TTL expiry")
}

func TestCachedAdminChecker_DifferentUsers(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	alice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	bob := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), alice)
	checker.IsAdmin(context.Background(), bob)

	assert.Equal(t, 2, delegate.getCalls(), "different users should be separate cache entries")
}

func TestCachedAdminChecker_SameUserDifferentGroups(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	adminAlice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	userAlice := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	checker.IsAdmin(context.Background(), adminAlice)
	checker.IsAdmin(context.Background(), userAlice)

	assert.Equal(t, 2, delegate.getCalls(), "same user with different groups should be separate cache entries")
}

func TestCachedAdminChecker_GroupOrderIrrelevant(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"b", "a", "c"}}
	user2 := &token.UserContext{Username: "alice", Groups: []string{"c", "a", "b"}}

	checker.IsAdmin(context.Background(), user1)
	checker.IsAdmin(context.Background(), user2)

	assert.Equal(t, 1, delegate.getCalls(), "group order should not matter for cache key")
}

func TestCachedAdminChecker_NilUserReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	assert.False(t, checker.IsAdmin(context.Background(), nil))
	assert.Equal(t, 0, delegate.getCalls())
}

func TestCachedAdminChecker_EmptyUsernameReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user := &token.UserContext{Username: "", Groups: []string{"admins"}}
	assert.False(t, checker.IsAdmin(context.Background(), user))
	assert.Equal(t, 0, delegate.getCalls())
}

func TestCachedAdminChecker_NilCheckerReturnsFalse(t *testing.T) {
	var checker *auth.CachedAdminChecker
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	assert.False(t, checker.IsAdmin(context.Background(), user))
}

func TestCachedAdminChecker_Metrics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	checker := auth.NewCachedAdminChecker(delegate, time.Minute, reg, nil)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), user)

	hits, misses := checker.Metrics()
	assert.InDelta(t, 0, counterValue(t, hits), 0)
	assert.InDelta(t, 1, counterValue(t, misses), 0)

	checker.IsAdmin(context.Background(), user)

	assert.InDelta(t, 1, counterValue(t, hits), 0)
	assert.InDelta(t, 1, counterValue(t, misses), 0)
}

func TestCachedAdminChecker_ConcurrentAccess(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	var wg sync.WaitGroup
	var trueCount atomic.Int64

	for range 100 {
		wg.Go(func() {
			if checker.IsAdmin(context.Background(), user) {
				trueCount.Add(1)
			}
		})
	}

	wg.Wait()
	assert.Equal(t, int64(100), trueCount.Load(), "all calls should return true")
}

func TestCachedAdminChecker_NilDelegatePanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(nil, time.Minute, reg, nil)
	})
}

func TestCachedAdminChecker_NonPositiveTTLPanics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(delegate, 0, reg, nil)
	})
}

func TestCachedAdminChecker_FalseResultIsCached(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate)
	user := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	assert.False(t, checker.IsAdmin(context.Background(), user))
	assert.False(t, checker.IsAdmin(context.Background(), user))

	assert.Equal(t, 1, delegate.getCalls(), "false results should also be cached")
}

func TestCachedAdminChecker_EvictsExpiredEntries(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	fakeClock := testingclock.NewFakeClock(time.Now())
	checker := auth.NewCachedAdminChecker(delegate, 10*time.Second, reg, fakeClock)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	user2 := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	// Cache user1 at t=0
	checker.IsAdmin(context.Background(), user1)
	require.Equal(t, 1, delegate.getCalls())

	// At t=11s user1's entry is expired; calling user2 triggers eviction of user1
	fakeClock.Step(11 * time.Second)
	checker.IsAdmin(context.Background(), user2)
	require.Equal(t, 2, delegate.getCalls())

	// user1's entry was evicted, so this should call the delegate again
	checker.IsAdmin(context.Background(), user1)
	assert.Equal(t, 3, delegate.getCalls(), "evicted entry should require fresh delegate call")
}
