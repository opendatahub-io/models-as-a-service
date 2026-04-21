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

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func newTestChecker(delegate *mockDelegate, ttl time.Duration) *auth.CachedAdminChecker {
	reg := prometheus.NewRegistry()
	return auth.NewCachedAdminChecker(delegate, ttl, reg)
}

func TestCachedAdminChecker_CacheHit(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)
	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	assert.True(t, checker.IsAdmin(context.Background(), user))
	assert.True(t, checker.IsAdmin(context.Background(), user))

	assert.Equal(t, 1, delegate.getCalls(), "delegate should be called only once")
}

func TestCachedAdminChecker_CacheMiss(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate, time.Minute)
	user := &token.UserContext{Username: "bob", Groups: []string{"users"}}

	assert.False(t, checker.IsAdmin(context.Background(), user))
	assert.Equal(t, 1, delegate.getCalls())
}

func TestCachedAdminChecker_TTLExpiry(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	checker := auth.NewCachedAdminChecker(delegate, 30*time.Second, reg)

	now := time.Now()
	checker.SetNowFunc(func() time.Time { return now })

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 1, delegate.getCalls())

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 1, delegate.getCalls(), "should use cache before TTL")

	checker.SetNowFunc(func() time.Time { return now.Add(31 * time.Second) })

	checker.IsAdmin(context.Background(), user)
	assert.Equal(t, 2, delegate.getCalls(), "should call delegate after TTL expiry")
}

func TestCachedAdminChecker_DifferentUsers(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

	alice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	bob := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), alice)
	checker.IsAdmin(context.Background(), bob)

	assert.Equal(t, 2, delegate.getCalls(), "different users should be separate cache entries")
}

func TestCachedAdminChecker_SameUserDifferentGroups(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

	adminAlice := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	userAlice := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	checker.IsAdmin(context.Background(), adminAlice)
	checker.IsAdmin(context.Background(), userAlice)

	assert.Equal(t, 2, delegate.getCalls(), "same user with different groups should be separate cache entries")
}

func TestCachedAdminChecker_GroupOrderIrrelevant(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

	user1 := &token.UserContext{Username: "alice", Groups: []string{"b", "a", "c"}}
	user2 := &token.UserContext{Username: "alice", Groups: []string{"c", "a", "b"}}

	checker.IsAdmin(context.Background(), user1)
	checker.IsAdmin(context.Background(), user2)

	assert.Equal(t, 1, delegate.getCalls(), "group order should not matter for cache key")
}

func TestCachedAdminChecker_NilUserReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

	assert.False(t, checker.IsAdmin(context.Background(), nil))
	assert.Equal(t, 0, delegate.getCalls())
}

func TestCachedAdminChecker_EmptyUsernameReturnsFalse(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

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
	checker := auth.NewCachedAdminChecker(delegate, time.Minute, reg)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), user)

	hits, misses := checker.Metrics()
	assert.Equal(t, float64(0), counterValue(hits))
	assert.Equal(t, float64(1), counterValue(misses))

	checker.IsAdmin(context.Background(), user)

	assert.Equal(t, float64(1), counterValue(hits))
	assert.Equal(t, float64(1), counterValue(misses))
}

func TestCachedAdminChecker_ConcurrentAccess(t *testing.T) {
	delegate := &mockDelegate{response: true}
	checker := newTestChecker(delegate, time.Minute)

	user := &token.UserContext{Username: "alice", Groups: []string{"admins"}}

	var wg sync.WaitGroup
	var trueCount atomic.Int64

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if checker.IsAdmin(context.Background(), user) {
				trueCount.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, int64(100), trueCount.Load(), "all calls should return true")
}

func TestCachedAdminChecker_NilDelegatePanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(nil, time.Minute, reg)
	})
}

func TestCachedAdminChecker_NonPositiveTTLPanics(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	assert.Panics(t, func() {
		auth.NewCachedAdminChecker(delegate, 0, reg)
	})
}

func TestCachedAdminChecker_FalseResultIsCached(t *testing.T) {
	delegate := &mockDelegate{response: false}
	checker := newTestChecker(delegate, time.Minute)
	user := &token.UserContext{Username: "alice", Groups: []string{"users"}}

	assert.False(t, checker.IsAdmin(context.Background(), user))
	assert.False(t, checker.IsAdmin(context.Background(), user))

	assert.Equal(t, 1, delegate.getCalls(), "false results should also be cached")
}

func TestCachedAdminChecker_EvictsExpiredEntries(t *testing.T) {
	delegate := &mockDelegate{response: true}
	reg := prometheus.NewRegistry()
	checker := auth.NewCachedAdminChecker(delegate, 10*time.Second, reg)

	now := time.Now()
	checker.SetNowFunc(func() time.Time { return now })

	user1 := &token.UserContext{Username: "alice", Groups: []string{"admins"}}
	user2 := &token.UserContext{Username: "bob", Groups: []string{"admins"}}

	checker.IsAdmin(context.Background(), user1)

	checker.SetNowFunc(func() time.Time { return now.Add(11 * time.Second) })

	checker.IsAdmin(context.Background(), user2)

	require.Equal(t, 3, delegate.getCalls())

	checker.SetNowFunc(func() time.Time { return now })
	checker.IsAdmin(context.Background(), user1)
	assert.Equal(t, 4, delegate.getCalls(), "evicted entry should require fresh delegate call")
}
