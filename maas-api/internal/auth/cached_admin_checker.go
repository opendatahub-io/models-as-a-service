package auth

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/utils/clock"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type adminChecker interface {
	IsAdmin(ctx context.Context, user *token.UserContext) bool
}

type cacheEntry struct {
	isAdmin   bool
	expiresAt time.Time
}

type CachedAdminChecker struct {
	delegate adminChecker
	ttl      time.Duration
	clock    clock.Clock

	mu    sync.RWMutex
	cache map[string]cacheEntry

	hits   prometheus.Counter
	misses prometheus.Counter
}

func NewCachedAdminChecker(delegate adminChecker, ttl time.Duration, reg prometheus.Registerer, clk clock.Clock) *CachedAdminChecker {
	if delegate == nil {
		panic("delegate cannot be nil for CachedAdminChecker")
	}
	if ttl <= 0 {
		panic("ttl must be positive for CachedAdminChecker")
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	if clk == nil {
		clk = clock.RealClock{}
	}

	factory := promauto.With(reg)
	return &CachedAdminChecker{
		delegate: delegate,
		ttl:      ttl,
		clock:    clk,
		cache:    make(map[string]cacheEntry),
		hits: factory.NewCounter(prometheus.CounterOpts{
			Name: "sar_cache_hits_total",
			Help: "Total number of SAR admin check cache hits.",
		}),
		misses: factory.NewCounter(prometheus.CounterOpts{
			Name: "sar_cache_misses_total",
			Help: "Total number of SAR admin check cache misses.",
		}),
	}
}

func (c *CachedAdminChecker) IsAdmin(ctx context.Context, user *token.UserContext) bool {
	if c == nil || user == nil || user.Username == "" {
		return false
	}

	key := cacheKey(user)
	now := c.clock.Now()

	c.mu.RLock()
	entry, ok := c.cache[key]
	c.mu.RUnlock()

	if ok && now.Before(entry.expiresAt) {
		c.hits.Inc()
		return entry.isAdmin
	}

	result := c.delegate.IsAdmin(ctx, user)

	c.mu.Lock()
	c.cache[key] = cacheEntry{
		isAdmin:   result,
		expiresAt: now.Add(c.ttl),
	}
	c.evictExpiredLocked(now)
	c.mu.Unlock()

	c.misses.Inc()
	return result
}

func (c *CachedAdminChecker) Metrics() (hits, misses prometheus.Counter) {
	return c.hits, c.misses
}

func (c *CachedAdminChecker) evictExpiredLocked(now time.Time) {
	for k, v := range c.cache {
		if now.After(v.expiresAt) {
			delete(c.cache, k)
		}
	}
}

func cacheKey(user *token.UserContext) string {
	sorted := make([]string, len(user.Groups))
	copy(sorted, user.Groups)
	slices.Sort(sorted)
	return user.Username + "\x00" + strings.Join(sorted, "\x00")
}
