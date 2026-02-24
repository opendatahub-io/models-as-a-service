package api_keys

import (
	"context"
	"encoding/json"
	"errors"
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
	metadata   ApiKeyMetadata
	username   string
	keyHash    string
	expiresAt  time.Time
	lastUsedAt *time.Time
}

// NewMockStore creates a new in-memory mock store for testing.
func NewMockStore() *MockStore {
	return &MockStore{
		keys: make(map[string]*storedKey),
	}
}

// Compile-time check that MockStore implements MetadataStore.
var _ MetadataStore = (*MockStore)(nil)

func (m *MockStore) Add(ctx context.Context, username string, apiKey *APIKey, userGroups string) error {
	if apiKey.JTI == "" {
		return ErrEmptyJTI
	}
	if apiKey.Name == "" {
		return ErrEmptyName
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var createdAt time.Time
	if apiKey.IssuedAt > 0 {
		createdAt = time.Unix(apiKey.IssuedAt, 0).UTC()
	} else {
		createdAt = time.Now().UTC()
	}

	// Parse user groups from JSON
	var groups []string
	if userGroups != "" {
		_ = json.Unmarshal([]byte(userGroups), &groups)
	}

	m.keys[apiKey.JTI] = &storedKey{
		metadata: ApiKeyMetadata{
			ID:                 apiKey.JTI,
			Name:               apiKey.Name,
			Description:        apiKey.Description,
			OriginalUserGroups: groups,
			Status:             TokenStatusActive,
			CreationDate:       createdAt.Format(time.RFC3339),
		},
		username:  username,
		expiresAt: time.Unix(apiKey.ExpiresAt, 0).UTC(),
	}

	return nil
}

// AddKey stores an API key with hash-only storage (no plaintext).
// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
func (m *MockStore) AddKey(ctx context.Context, username, keyID, keyHash, keyPrefix, name, description, userGroups string, expiresAt *time.Time) error {
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

	// Parse user groups from JSON
	var groups []string
	if userGroups != "" {
		_ = json.Unmarshal([]byte(userGroups), &groups)
	}

	m.keys[keyID] = &storedKey{
		metadata: ApiKeyMetadata{
			ID:                 keyID,
			Name:               name,
			Description:        description,
			KeyPrefix:          keyPrefix,
			OriginalUserGroups: groups,
			Status:             TokenStatusActive,
			CreationDate:       time.Now().UTC().Format(time.RFC3339),
		},
		username:  username,
		keyHash:   keyHash,
		expiresAt: expiresAtTime,
	}

	return nil
}

// List returns a paginated list of API keys for a user.
// Pagination is mandatory - no unbounded queries allowed.
func (m *MockStore) List(ctx context.Context, username string, params PaginationParams) (*PaginatedResult, error) {
	// Validate params (same as PostgresStore)
	if params.Limit < 1 || params.Limit > 100 {
		return nil, errors.New("limit must be between 1 and 100")
	}
	if params.Offset < 0 {
		return nil, errors.New("offset must be non-negative")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Get all keys for user
	allKeys := make([]ApiKeyMetadata, 0)
	now := time.Now().UTC()

	for _, k := range m.keys {
		if k.username != username {
			continue
		}

		meta := k.metadata
		// Auto-expire logic
		if meta.Status == TokenStatusActive && !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
			meta.Status = TokenStatusExpired
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
			Keys:    []ApiKeyMetadata{},
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

func (m *MockStore) Get(ctx context.Context, keyID string) (*ApiKeyMetadata, error) {
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
	if meta.Status == TokenStatusActive && !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
		meta.Status = TokenStatusExpired
	}
	if !k.expiresAt.IsZero() {
		meta.ExpirationDate = k.expiresAt.Format(time.RFC3339)
	}
	if k.lastUsedAt != nil {
		meta.LastUsedAt = k.lastUsedAt.Format(time.RFC3339)
	}

	return &meta, nil
}

func (m *MockStore) GetByHash(ctx context.Context, keyHash string) (*ApiKeyMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, k := range m.keys {
		if k.keyHash == keyHash {
			// Check expiration and auto-update status if expired
			now := time.Now().UTC()
			if !k.expiresAt.IsZero() && k.expiresAt.Before(now) {
				if k.metadata.Status == TokenStatusActive {
					k.metadata.Status = TokenStatusExpired
				}
			}

			// Reject revoked/expired keys
			if k.metadata.Status == TokenStatusRevoked || k.metadata.Status == TokenStatusExpired {
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

func (m *MockStore) InvalidateAll(ctx context.Context, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, k := range m.keys {
		if k.username == username && k.metadata.Status == TokenStatusActive {
			k.metadata.Status = TokenStatusRevoked
		}
	}

	return nil
}

func (m *MockStore) Revoke(ctx context.Context, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[keyID]
	if !ok {
		return ErrKeyNotFound
	}

	k.metadata.Status = TokenStatusRevoked
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
