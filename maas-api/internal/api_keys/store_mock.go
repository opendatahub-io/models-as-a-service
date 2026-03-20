package api_keys

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// MockStore implements MetadataStore for testing purposes.
// It stores data in memory and is safe for concurrent use.
type MockStore struct {
	mu   sync.RWMutex
	keys map[string]*storedKey // keyed by ID
}

type storedKey struct {
	metadata   ApiKey
	username   string
	keyHash    string
	expiresAt  time.Time
	lastUsedAt *time.Time
	ephemeral  bool
}

// NewMockStore creates a new in-memory mock store for testing.
func NewMockStore() *MockStore {
	return &MockStore{
		keys: make(map[string]*storedKey),
	}
}

// Compile-time check that MockStore implements MetadataStore.
var _ MetadataStore = (*MockStore)(nil)

// AddKey stores an API key with hash-only storage (no plaintext).
// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
// ephemeral marks the key as short-lived for programmatic use.
// Note: keyPrefix is NOT stored (security - reduces brute-force attack surface).
func (m *MockStore) AddKey(ctx context.Context, username, keyID, keyHash, name, description string, userGroups []string, expiresAt *time.Time, ephemeral bool) error {
	if keyID == "" {
		return ErrEmptyJTI
	}
	if name == "" {
		return ErrEmptyName
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var expiresAtTime time.Time
	if expiresAt != nil {
		expiresAtTime = *expiresAt
	}

	// userGroups is already []string - no parsing needed
	m.keys[keyID] = &storedKey{
		metadata: ApiKey{
			ID:           keyID,
			Name:         name,
			Description:  description,
			Groups:       userGroups,
			Status:       StatusActive,
			CreationDate: time.Now().UTC().Format(time.RFC3339),
			Ephemeral:    ephemeral,
		},
		username:  username,
		keyHash:   keyHash,
		expiresAt: expiresAtTime,
		ephemeral: ephemeral,
	}

	return nil
}

// List returns a paginated list of API keys with optional filtering.
// Pagination is mandatory - no unbounded queries allowed.
// username can be empty (all users) or specific username.
// statuses can filter by status - empty means all statuses.
// Note: Ephemeral keys are excluded by default (use Search with IncludeEphemeral for full control).
func (m *MockStore) List(ctx context.Context, username string, params PaginationParams, statuses []string) (*PaginatedResult, error) {
	// Validate params (same as PostgresStore)
	if params.Limit < 1 || params.Limit > 100 {
		return nil, errors.New("limit must be between 1 and 100")
	}
	if params.Offset < 0 {
		return nil, errors.New("offset must be non-negative")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Get all keys matching filters
	allKeys := make([]ApiKey, 0)
	now := time.Now().UTC()

	for _, k := range m.keys {
		// Exclude ephemeral keys by default
		if k.ephemeral {
			continue
		}

		// Filter by username (empty = all users)
		if username != "" && k.username != username {
			continue
		}

		meta := k.metadata
		meta.Username = k.username // Set username field

		// Auto-expire logic
		if meta.Status == StatusActive && !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
			meta.Status = StatusExpired
		}

		// Filter by status
		if len(statuses) > 0 && !slices.Contains(statuses, string(meta.Status)) {
			continue
		}

		if !k.expiresAt.IsZero() {
			meta.ExpirationDate = k.expiresAt.Format(time.RFC3339)
		}
		if k.lastUsedAt != nil {
			meta.LastUsedAt = k.lastUsedAt.Format(time.RFC3339)
		}
		allKeys = append(allKeys, meta)
	}

	// Apply pagination
	start := params.Offset
	end := start + params.Limit + 1 // Fetch limit+1 for hasMore check

	if start >= len(allKeys) {
		return &PaginatedResult{
			Keys:    []ApiKey{},
			HasMore: false,
		}, nil
	}

	if end > len(allKeys) {
		end = len(allKeys)
	}

	pagedKeys := allKeys[start:end]
	hasMore := len(pagedKeys) > params.Limit

	if hasMore {
		pagedKeys = pagedKeys[:params.Limit]
	}

	return &PaginatedResult{
		Keys:    pagedKeys,
		HasMore: hasMore,
	}, nil
}

// filterKeys applies username, status, and ephemeral filters to API keys.
func (m *MockStore) filterKeys(username string, statusFilters []string, includeEphemeral bool, now time.Time) []ApiKey {
	filtered := make([]ApiKey, 0, len(m.keys))

	for _, k := range m.keys {
		// Filter ephemeral keys unless explicitly included
		if !includeEphemeral && k.ephemeral {
			continue
		}

		// Filter by username
		if username != "" && k.username != username {
			continue
		}

		meta := k.metadata
		meta.Username = k.username

		// Auto-expire logic
		if meta.Status == StatusActive && !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
			meta.Status = StatusExpired
		}

		// Filter by status
		if len(statusFilters) > 0 {
			found := false
			for _, status := range statusFilters {
				if strings.TrimSpace(status) == string(meta.Status) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Set expiration and last used timestamps
		if !k.expiresAt.IsZero() {
			meta.ExpirationDate = k.expiresAt.Format(time.RFC3339)
		}
		if k.lastUsedAt != nil {
			meta.LastUsedAt = k.lastUsedAt.Format(time.RFC3339)
		}

		filtered = append(filtered, meta)
	}

	return filtered
}

// compareTimestampFields compares two timestamp strings with NULL handling.
// Returns: comparison result (-1, 0, 1) and a boolean indicating if NULL sorting should apply.
func compareTimestampFields(val1, val2 string) (int, bool) {
	// Both NULL - equal
	if val1 == "" && val2 == "" {
		return 0, false
	}
	// NULL values sort last
	if val1 == "" {
		return 0, true // Caller should return (order == SortOrderDesc)
	}
	if val2 == "" {
		return 0, true // Caller should return (order == SortOrderAsc)
	}

	// Both non-NULL, parse and compare
	t1, err1 := time.Parse(time.RFC3339, val1)
	t2, err2 := time.Parse(time.RFC3339, val2)
	if err1 != nil || err2 != nil {
		// Fallback to string comparison
		return strings.Compare(val1, val2), false
	}
	return t1.Compare(t2), false
}

// compareKeys compares two API keys based on the sort field and order.
func compareKeys(key1, key2 ApiKey, sortBy, order string) bool {
	var cmp int

	switch sortBy {
	case "created_at":
		// Both fields are always populated, safe to parse
		t1, err1 := time.Parse(time.RFC3339, key1.CreationDate)
		t2, err2 := time.Parse(time.RFC3339, key2.CreationDate)
		if err1 != nil || err2 != nil {
			cmp = strings.Compare(key1.CreationDate, key2.CreationDate)
		} else {
			cmp = t1.Compare(t2)
		}

	case "expires_at":
		var handleNull bool
		cmp, handleNull = compareTimestampFields(key1.ExpirationDate, key2.ExpirationDate)
		if handleNull {
			// NULL sorting logic
			if key1.ExpirationDate == "" {
				return order == SortOrderDesc
			}
			return order == SortOrderAsc
		}

	case "last_used_at":
		var handleNull bool
		cmp, handleNull = compareTimestampFields(key1.LastUsedAt, key2.LastUsedAt)
		if handleNull {
			// NULL sorting logic
			if key1.LastUsedAt == "" {
				return order == SortOrderDesc
			}
			return order == SortOrderAsc
		}

	case "name":
		cmp = strings.Compare(key1.Name, key2.Name)
	}

	// Apply sort order
	if order == SortOrderDesc {
		return cmp > 0
	}
	return cmp < 0
}

// applyPagination applies offset and limit to the result set.
func applyPagination(keys []ApiKey, offset, limit int) ([]ApiKey, bool) {
	// Handle offset out of bounds
	if offset >= len(keys) {
		return []ApiKey{}, false
	}

	// Apply offset
	keys = keys[offset:]

	// Check for more results and apply limit
	hasMore := len(keys) > limit
	if hasMore {
		keys = keys[:limit]
	}

	return keys, hasMore
}

// Search implements flexible API key search with filtering, sorting, pagination.
// Ephemeral keys are excluded by default unless IncludeEphemeral filter is set to true.
func (m *MockStore) Search(
	ctx context.Context,
	username string,
	filters *SearchFilters,
	sortParams *SortParams,
	pagination *PaginationParams,
) (*PaginatedResult, error) {
	// Validate pagination
	if pagination.Limit < 1 || pagination.Limit > MaxLimit {
		return nil, errors.New("limit must be between 1 and 100")
	}
	if pagination.Offset < 0 {
		return nil, errors.New("offset must be non-negative")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Determine if ephemeral keys should be included
	includeEphemeral := filters.IncludeEphemeral != nil && *filters.IncludeEphemeral

	// Filter keys by username, status, and ephemeral
	now := time.Now().UTC()
	allKeys := m.filterKeys(username, filters.Status, includeEphemeral, now)

	// Sort keys
	sort.Slice(allKeys, func(i, j int) bool {
		return compareKeys(allKeys[i], allKeys[j], sortParams.By, sortParams.Order)
	})

	// Apply pagination
	keys, hasMore := applyPagination(allKeys, pagination.Offset, pagination.Limit)

	return &PaginatedResult{
		Keys:    keys,
		HasMore: hasMore,
	}, nil
}

func (m *MockStore) Get(ctx context.Context, keyID string) (*ApiKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k, ok := m.keys[keyID]
	if !ok {
		return nil, ErrKeyNotFound
	}

	meta := k.metadata
	meta.Username = k.username

	// Compute status
	now := time.Now().UTC()
	if meta.Status == StatusActive && !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
		meta.Status = StatusExpired
	}
	if !k.expiresAt.IsZero() {
		meta.ExpirationDate = k.expiresAt.Format(time.RFC3339)
	}
	if k.lastUsedAt != nil {
		meta.LastUsedAt = k.lastUsedAt.Format(time.RFC3339)
	}

	return &meta, nil
}

func (m *MockStore) GetByHash(ctx context.Context, keyHash string) (*ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, k := range m.keys {
		if k.keyHash == keyHash {
			// Check expiration and auto-update status if expired
			now := time.Now().UTC()
			if !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
				if k.metadata.Status == StatusActive {
					k.metadata.Status = StatusExpired
				}
			}

			// Reject revoked/expired keys
			if k.metadata.Status == StatusRevoked || k.metadata.Status == StatusExpired {
				return nil, ErrInvalidKey
			}

			meta := k.metadata
			meta.Username = k.username
			if k.lastUsedAt != nil {
				meta.LastUsedAt = k.lastUsedAt.Format(time.RFC3339)
			}
			return &meta, nil
		}
	}

	return nil, ErrKeyNotFound
}

func (m *MockStore) InvalidateAll(ctx context.Context, username string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, k := range m.keys {
		if k.username == username && k.metadata.Status == StatusActive {
			k.metadata.Status = StatusRevoked
			count++
		}
	}

	return count, nil
}

func (m *MockStore) Revoke(ctx context.Context, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[keyID]
	if !ok {
		return ErrKeyNotFound
	}

	// Match PostgreSQL behavior: only revoke keys with status 'active'
	if k.metadata.Status != StatusActive {
		return ErrKeyNotFound
	}

	k.metadata.Status = StatusRevoked
	return nil
}

func (m *MockStore) UpdateLastUsed(ctx context.Context, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[keyID]
	if !ok {
		return ErrKeyNotFound
	}

	now := time.Now().UTC()
	k.lastUsedAt = &now
	return nil
}

func (m *MockStore) Close() error {
	return nil
}
