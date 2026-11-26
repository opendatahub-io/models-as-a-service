package auth

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Clock returns the current time. It is an interface so tests can control
// TTL expiry deterministically.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// CacheStats reports cache effectiveness counters. They exist so the cache
// can be verified as effective (repeated tokens produce hits, not API calls)
// and so observability wiring (metrics endpoint) can expose them without
// touching the cache implementation.
type CacheStats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

// ReviewCacheOptions configures a ReviewCache.
type ReviewCacheOptions struct {
	// TTL is how long a successful auth decision is reused. Zero disables
	// caching entirely (every request reviews the token).
	TTL time.Duration
	// NegativeTTL is how long a rejected auth decision (unauthenticated
	// token or unauthorized caller) is reused, keeping rejected tokens
	// fast-path cheap without long-locking a denial. Must be positive when
	// TTL is positive.
	NegativeTTL time.Duration
	// MaxEntries bounds the cache size (LRU eviction). Must be positive when
	// TTL is positive.
	MaxEntries int
	// Clock overrides time; nil uses the real clock.
	Clock Clock
}

type cacheEntry struct {
	result    AuthResult
	expiresAt time.Time
}

type cacheKeyNode struct {
	key   string
	entry cacheEntry
	elem  *list.Element
}

// ReviewCache caches Reviewer decisions per bearer token so repeated requests
// do not generate per-request TokenReview/SubjectAccessReview API-server
// calls. It wraps a delegate Reviewer (e.g. TokenReviewer).
//
// Keys are SHA-256 hashes of the token: raw bearer tokens are never stored,
// and the key depends only on the token, so per-request data cannot split
// entries. Entry TTLs are the lesser of the configured TTL and the token's
// remaining validity, so a cached allow is never used past the token's
// expiry. Rejected decisions are cached for the shorter NegativeTTL. Errors
// from the delegate are never cached (fail closed): an API-server failure
// keeps producing 401/500 responses exactly as without the cache.
//
// A cached decision can mask an RBAC change (e.g. a revoked admin group or
// tenant role) until its TTL expires; the default TTL matches the maas-api
// SAR admin-check cache (30s).
type ReviewCache struct {
	delegate    Reviewer
	ttl         time.Duration
	negativeTTL time.Duration
	maxEntries  int
	clock       Clock

	mu      sync.Mutex
	entries map[string]*cacheKeyNode
	lru     *list.List // of *cacheKeyNode; front = most recently used

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// NewReviewCache wraps delegate with a decision cache. With
// opts.TTL <= 0 the returned cache is a pass-through (caching disabled).
// Panics when TTL is positive but NegativeTTL or MaxEntries is not — a
// programmer error caught at startup.
func NewReviewCache(delegate Reviewer, opts ReviewCacheOptions) *ReviewCache {
	if delegate == nil {
		panic("delegate cannot be nil for ReviewCache")
	}
	if opts.TTL > 0 {
		if opts.NegativeTTL <= 0 {
			panic("negativeTTL must be positive when caching is enabled")
		}
		if opts.MaxEntries <= 0 {
			panic("maxEntries must be positive when caching is enabled")
		}
	}
	clk := opts.Clock
	if clk == nil {
		clk = realClock{}
	}
	return &ReviewCache{
		delegate:    delegate,
		ttl:         opts.TTL,
		negativeTTL: opts.NegativeTTL,
		maxEntries:  opts.MaxEntries,
		clock:       clk,
		entries:     make(map[string]*cacheKeyNode),
		lru:         list.New(),
	}
}

// Review returns the cached decision for the token, consulting the delegate
// on a miss. Delegate errors are returned uncached.
func (c *ReviewCache) Review(ctx context.Context, token string) (AuthResult, error) {
	if c.ttl <= 0 {
		return c.delegate.Review(ctx, token)
	}

	key := tokenKey(token)
	now := c.clock.Now()

	c.mu.Lock()
	if node, ok := c.entries[key]; ok {
		if now.Before(node.entry.expiresAt) {
			// Copy the result while holding the mutex: a concurrent
			// review of the same token may replace node.entry (under the
			// mutex) while this read is in flight.
			result := node.entry.result
			c.lru.MoveToFront(node.elem)
			c.mu.Unlock()
			c.hits.Add(1)
			return result, nil
		}
		// Expired: drop the stale entry before re-reviewing.
		c.removeLocked(node)
	}
	c.mu.Unlock()

	c.misses.Add(1)
	result, err := c.delegate.Review(ctx, token)
	if err != nil {
		return AuthResult{}, err
	}

	if result.adminCheckFailed {
		// Degraded decision (admin check errored): serving it once is the
		// accepted fallback, but caching it would pin non-admin visibility
		// on an administrator until the TTL expired after the API server
		// recovered.
		return result, nil
	}

	ttl := c.ttl
	if !result.Authenticated || !result.Authorized {
		ttl = c.negativeTTL
	}
	// The TokenReview API does not return the token's remaining lifetime,
	// so derive it from the JWT exp claim when present. The claim is read
	// without verification — the delegate's TokenReview already
	// authenticated the token — and is only ever used to shorten the TTL.
	// A present-but-unparseable exp claim cannot bound the decision's
	// validity, so caching is skipped for it (ttl 0 below).
	exp, hasExp, expErr := tokenExpiry(token)
	switch {
	case expErr != nil:
		ttl = 0
	case hasExp:
		if until := exp.Sub(now); until < ttl {
			ttl = until
		}
	}
	if ttl <= 0 {
		return result, nil
	}

	c.mu.Lock()
	if node, ok := c.entries[key]; ok {
		// Another request may have reviewed the same token concurrently;
		// refresh the existing entry instead of duplicating it.
		node.entry = cacheEntry{result: result, expiresAt: now.Add(ttl)}
		c.lru.MoveToFront(node.elem)
	} else {
		for c.lru.Len() >= c.maxEntries {
			oldest := c.lru.Back()
			if oldest == nil {
				break
			}
			if node, ok := oldest.Value.(*cacheKeyNode); ok {
				c.removeLocked(node)
				c.evictions.Add(1)
			} else {
				c.lru.Remove(oldest)
			}
		}
		node := &cacheKeyNode{
			key:   key,
			entry: cacheEntry{result: result, expiresAt: now.Add(ttl)},
		}
		node.elem = c.lru.PushFront(node)
		c.entries[key] = node
	}
	c.mu.Unlock()

	return result, nil
}

// Stats returns the cache effectiveness counters. Hits + Misses counts every
// Review call while caching is enabled; Evictions counts LRU evictions.
func (c *ReviewCache) Stats() CacheStats {
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}

// removeLocked deletes an entry; c.mu must be held.
func (c *ReviewCache) removeLocked(node *cacheKeyNode) {
	c.lru.Remove(node.elem)
	delete(c.entries, node.key)
}

// tokenKey hashes the raw bearer token so tokens are never stored in the
// cache and the key cannot be derived from request-varying data.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenExpiry extracts the "exp" claim from a JWT-formatted bearer token
// without verifying it. It returns:
//   - (expiry, true, nil) when the token is a JWT carrying a usable exp
//     claim, including non-integer NumericDates (RFC 7519), floored to the
//     second so the cap can never extend past the token's validity;
//   - (zero, false, nil) for non-JWT tokens or tokens without an exp claim,
//     in which case the configured TTL applies unchanged;
//   - an error when a JWT carries an exp claim that cannot be parsed as a
//     number — the caller must not cache such a decision, since its validity
//     cannot be bounded.
func tokenExpiry(token string) (time.Time, bool, error) {
	claims, ok := jwtClaims(token)
	if !ok {
		return time.Time{}, false, nil
	}
	raw, present := claims["exp"]
	if !present {
		return time.Time{}, false, nil
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return time.Time{}, false, fmt.Errorf("exp claim is not a number: %s", raw)
	}
	seconds, err := num.Float64()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("exp claim is not a valid number: %s", raw)
	}
	return time.Unix(int64(math.Floor(seconds)), 0).UTC(), true, nil
}

// jwtClaims decodes the claims of a JWT-formatted token without verifying
// it. It returns false when the token is not a three-part JWT with a JSON
// payload (e.g. an opaque OAuth token); such tokens carry no exp
// information and the configured TTL applies.
func jwtClaims(token string) (map[string]json.RawMessage, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}
