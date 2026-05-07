package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type adminChecker interface {
	IsAdmin(ctx context.Context, user *token.UserContext) (bool, error)
}

type cacheEntry struct {
	isAdmin   bool
	expiresAt time.Time
}

type CachedAdminChecker struct {
	delegate    adminChecker
	ttl         time.Duration
	negativeTTL time.Duration
	maxSize     int
	clock       clock.Clock

	mu    sync.RWMutex
	cache map[string]cacheEntry

	hits   prometheus.Counter
	misses prometheus.Counter
}

func NewCachedAdminChecker(delegate adminChecker, ttl time.Duration, negativeTTL time.Duration, maxSize int, reg prometheus.Registerer, clk clock.Clock) *CachedAdminChecker {
	if delegate == nil {
		panic("delegate cannot be nil for CachedAdminChecker")
	}
	if ttl <= 0 {
		panic("ttl must be positive for CachedAdminChecker")
	}
	if negativeTTL <= 0 {
		panic("negativeTTL must be positive for CachedAdminChecker")
	}
	if maxSize <= 0 {
		panic("maxSize must be positive for CachedAdminChecker")
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	if clk == nil {
		clk = clock.RealClock{}
	}

	return &CachedAdminChecker{
		delegate:    delegate,
		ttl:         ttl,
		negativeTTL: negativeTTL,
		maxSize:     maxSize,
		clock:       clk,
		cache:       make(map[string]cacheEntry),
		hits: registerOrReuseCounter(reg, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sar_cache_hits_total",
			Help: "Total number of SAR admin check cache hits.",
		})),
		misses: registerOrReuseCounter(reg, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sar_cache_misses_total",
			Help: "Total number of SAR admin check cache misses.",
		})),
	}
}

func (c *CachedAdminChecker) IsAdmin(ctx context.Context, user *token.UserContext) (bool, error) {
	if c == nil || user == nil || user.Username == "" {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	key := cacheKey(user)
	now := c.clock.Now()

	c.mu.RLock()
	entry, ok := c.cache[key]
	c.mu.RUnlock()

	if ok && now.Before(entry.expiresAt) {
		c.hits.Inc()
		return entry.isAdmin, nil
	}

	result, err := c.delegate.IsAdmin(ctx, user)

	if err != nil || ctx.Err() != nil {
		c.misses.Inc()
		if err != nil {
			return false, err
		}
		return false, ctx.Err()
	}

	ttl := c.ttl
	if !result {
		ttl = c.negativeTTL
	}

	storeTime := c.clock.Now()

	c.mu.Lock()
	c.evictExpiredLocked(storeTime)
	if len(c.cache) < c.maxSize {
		c.cache[key] = cacheEntry{
			isAdmin:   result,
			expiresAt: storeTime.Add(ttl),
		}
	}
	c.mu.Unlock()

	c.misses.Inc()
	return result, nil
}

//nolint:ireturn,nonamedreturns // Prometheus counters are inherently interface-typed; named returns clarify which is which.
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

//nolint:ireturn // prometheus.Counter is the canonical type for counters.
func registerOrReuseCounter(reg prometheus.Registerer, c prometheus.Counter) prometheus.Counter {
	err := reg.Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		existing, ok := are.ExistingCollector.(prometheus.Counter)
		if !ok {
			panic("existing collector is not a Counter")
		}
		return existing
	}
	panic(err)
}

func cacheKey(user *token.UserContext) string {
	sorted := make([]string, len(user.Groups))
	copy(sorted, user.Groups)
	slices.Sort(sorted)
	return user.Username + "\x00" + strings.Join(sorted, "\x00")
}
