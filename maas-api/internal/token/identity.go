package token

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

const (
	// MaxIdentityEnvelopeBytes is also used by trusted callbacks that carry the
	// same envelope in their JSON body. The limit is checked before decoding so
	// a malicious or misbehaving upstream cannot force unbounded allocation.
	MaxIdentityEnvelopeBytes = 16 * 1024

	// The envelope is intentionally flat. These limits bound parser work and
	// prevent a syntactically valid claim collection from becoming a resource
	// exhaustion vector.
	maxIdentityJSONDepth     = 6
	maxIdentityGroups        = 128
	maxIdentityOrganizations = 32
	maxOrganizationRoles     = 64
	maxAudiences             = 16
)

var (
	errIdentityTooLarge     = errors.New("identity envelope is too large")
	errIdentityDepth        = errors.New("identity envelope is too deeply nested")
	errIdentityDuplicateKey = errors.New("identity envelope contains a duplicate key")
	errIdentityTrailingData = errors.New("identity envelope contains trailing data")
	errIdentityMalformed    = errors.New("identity envelope is malformed")
)

// VerifiedIdentity is the only trusted SSO identity shape accepted by maas-api.
// Authorino has already verified the JWT; this envelope carries bounded claims
// and provenance separately.
type VerifiedIdentity struct {
	Source string         `json:"source"`
	Claims VerifiedClaims `json:"claims"`
}

type VerifiedClaims struct {
	// These claims are part of the strict token contract. Authorino has already
	// verified them before constructing this envelope.
	Issuer          string   `json:"iss"`
	Subject         string   `json:"sub"`
	Audience        Audience `json:"aud"`
	AuthorizedParty string   `json:"azp"`

	// Profile claims are optional display data for /v1/me. They are bounded
	// when present but never establish ownership or organization authority.
	Name              string   `json:"name,omitempty"`
	GivenName         string   `json:"given_name,omitempty"`
	FamilyName        string   `json:"family_name,omitempty"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Email             string   `json:"email,omitempty"`
	Groups            []string `json:"groups,omitempty"`
	// Organization memberships are the only future source of delegated-admin
	// decisions. Keeping them in typed, bounded entries prevents mixing one
	// organization's ID with another organization's roles.
	Organizations []OrganizationMembership `json:"https://redhat.com/maas/orgs,omitempty"`
}

type Audience struct {
	Value string
	Items []string
}

func (audience Audience) MarshalJSON() ([]byte, error) {
	if audience.Items != nil {
		return json.Marshal(audience.Items)
	}
	return json.Marshal(audience.Value)
}

func (audience *Audience) UnmarshalJSON(data []byte) error {
	// OIDC allows aud to be either a string or an array of strings. Preserve
	// which representation was supplied so validation can reject an envelope
	// that tries to populate both internal representations.
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		audience.Value = value
		return nil
	}

	var items []string
	if err := json.Unmarshal(data, &items); err != nil {
		return errors.New("audience must be a string or string array")
	}
	audience.Items = items
	return nil
}

type OrganizationMembership struct {
	// OrganizationID is canonicalized by validate before it can be used for
	// association, preventing equivalent noncanonical UUID spellings from
	// bypassing duplicate detection.
	OrganizationID   string   `json:"organization_id"`
	OrganizationName string   `json:"organization_name,omitempty"`
	Roles            []string `json:"roles,omitempty"`
}

// IdentityResponse contains only the identity fields exposed by GET /v1/me.
// It is deliberately separate from VerifiedIdentity so transport provenance
// and internal claim representations cannot leak into the public response.
type IdentityResponse struct {
	Issuer            string                   `json:"iss"`
	Subject           string                   `json:"sub"`
	Name              string                   `json:"name,omitempty"`
	GivenName         string                   `json:"given_name,omitempty"`
	FamilyName        string                   `json:"family_name,omitempty"`
	PreferredUsername string                   `json:"preferred_username,omitempty"`
	Email             string                   `json:"email,omitempty"`
	Groups            []string                 `json:"groups,omitempty"`
	Organizations     []OrganizationMembership `json:"https://redhat.com/maas/orgs,omitempty"`
}

// ParseIdentityEnvelope validates the fixed Authorino-to-maas-api identity
// transport. It never accepts a bearer JWT or falls back to unsigned parsing.
func ParseIdentityEnvelope(value string) (*VerifiedIdentity, error) {
	if len(value) > MaxIdentityEnvelopeBytes {
		return nil, errIdentityTooLarge
	}
	// encoding/json accepts duplicate object keys and ignores unknown fields by
	// default. The shape pass rejects both before the typed decode, while
	// DisallowUnknownFields provides a second defense in depth.
	if err := validateJSONShape(value); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()

	var identity VerifiedIdentity
	if err := decoder.Decode(&identity); err != nil {
		return nil, errIdentityMalformed
	}
	if err := identity.validate(); err != nil {
		return nil, err
	}
	return &identity, nil
}

func (identity *VerifiedIdentity) validate() error {
	if identity.Source != "oidc" {
		return errors.New("unsupported identity source")
	}

	claims := identity.Claims
	if !nonEmptyBoundedString(claims.Issuer, 2048) {
		return errors.New("invalid issuer claim")
	}
	if !nonEmptyBoundedString(claims.Subject, 256) {
		return errors.New("invalid subject claim")
	}
	if !nonEmptyBoundedString(claims.AuthorizedParty, 256) {
		return errors.New("invalid authorized party claim")
	}

	// Standard OIDC profile claims are optional. Requiring them would break
	// existing providers, but oversized values still fail closed so display
	// data cannot become an unbounded transport.
	if !optionalBoundedString(claims.Name, 256) {
		return errors.New("invalid name claim")
	}
	if !optionalBoundedString(claims.GivenName, 256) {
		return errors.New("invalid given name claim")
	}
	if !optionalBoundedString(claims.FamilyName, 256) {
		return errors.New("invalid family name claim")
	}
	if !optionalBoundedString(claims.PreferredUsername, 256) {
		return errors.New("invalid preferred username claim")
	}
	if !optionalBoundedString(claims.Email, 320) {
		return errors.New("invalid email claim")
	}

	if claims.Audience.Value == "" && len(claims.Audience.Items) == 0 {
		return errors.New("missing audience claim")
	}
	// A single decoded struct cannot faithfully represent both forms; choosing
	// one would make the other silently disappear. Reject the contradiction.
	if claims.Audience.Value != "" && len(claims.Audience.Items) != 0 {
		return errors.New("audience claim has both representations")
	}
	audiences := claims.Audience.Items
	if claims.Audience.Value != "" {
		audiences = []string{claims.Audience.Value}
	}
	if len(audiences) > maxAudiences {
		return errors.New("too many audiences")
	}
	for _, audience := range audiences {
		if !nonEmptyBoundedString(audience, 256) {
			return errors.New("invalid audience claim")
		}
	}

	if len(claims.Groups) > maxIdentityGroups {
		return errors.New("too many groups")
	}
	for _, group := range claims.Groups {
		if !nonEmptyBoundedString(group, 256) {
			return errors.New("invalid group claim")
		}
	}

	if len(claims.Organizations) > maxIdentityOrganizations {
		return errors.New("too many organizations")
	}
	organizationIDs := make(map[string]struct{}, len(claims.Organizations))
	for _, organization := range claims.Organizations {
		// Parse and re-render the UUID to require the canonical form. This also
		// prevents duplicate IDs that differ only in case or formatting.
		parsedID, err := uuid.Parse(organization.OrganizationID)
		if err != nil || parsedID.String() != organization.OrganizationID {
			return errors.New("invalid organization id")
		}
		if _, exists := organizationIDs[organization.OrganizationID]; exists {
			return errors.New("duplicate organization id")
		}
		organizationIDs[organization.OrganizationID] = struct{}{}

		// The name is optional display data for /v1/me. It is not used for
		// association or authorization, but still requires a transport bound.
		if !optionalBoundedString(organization.OrganizationName, 256) {
			return errors.New("invalid organization name claim")
		}
		if len(organization.Roles) > maxOrganizationRoles {
			return errors.New("too many organization roles")
		}
		for _, role := range organization.Roles {
			if !nonEmptyBoundedString(role, 128) {
				return errors.New("invalid organization role")
			}
		}
	}

	return nil
}

func nonEmptyBoundedString(value string, limit int) bool {
	return value != "" && len(value) <= limit
}

func optionalBoundedString(value string, limit int) bool {
	return value == "" || len(value) <= limit
}

func validateJSONShape(value string) error {
	// Walk the complete JSON value before decoding it into Go objects. This
	// gives deterministic failures for depth, duplicate keys, and trailing data
	// without allocating claim slices or maps first.
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := validateJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errIdentityTrailingData
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxIdentityJSONDepth {
		return errIdentityDepth
	}

	token, err := decoder.Token()
	if err != nil {
		return errIdentityMalformed
	}

	delim, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delim {
	case '{':
		seenKeys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errIdentityMalformed
			}
			key, ok := keyToken.(string)
			if !ok {
				return errIdentityMalformed
			}
			if _, exists := seenKeys[key]; exists {
				return errIdentityDuplicateKey
			}
			seenKeys[key] = struct{}{}
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errIdentityMalformed
	}

	if _, err := decoder.Token(); err != nil {
		return errIdentityMalformed
	}
	return nil
}

// ExtractVerifiedIdentityOptional preserves legacy requests when the identity
// envelope is absent. A malformed, duplicate, or oversized envelope fails closed.
func (h *Handler) ExtractVerifiedIdentityOptional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := validateOptionalIdentityHeaders(c.Request.Header); err != nil {
			h.rejectIdentity(c, err)
			return
		}

		value := c.Request.Header.Get(constant.HeaderIdentity)
		if value == "" {
			// Older gateways do not inject the new envelope. Preserve their
			// behavior and let the legacy username/group middleware run.
			c.Next()
			return
		}

		identity, err := ParseIdentityEnvelope(value)
		if err != nil {
			h.rejectIdentity(c, err)
			return
		}
		c.Set("verified_identity", identity)
		c.Next()
	}
}

// ExtractVerifiedIdentity requires the trusted SSO identity envelope. Unlike
// the optional variant, legacy username/group headers cannot authenticate an
// endpoint that is defined specifically for verified OIDC identity.
func (h *Handler) ExtractVerifiedIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := validateOptionalIdentityHeaders(c.Request.Header); err != nil {
			h.rejectIdentity(c, err)
			return
		}

		value := c.Request.Header.Get(constant.HeaderIdentity)
		if value == "" {
			h.rejectIdentity(c, errors.New("trusted identity is required"))
			return
		}

		identity, err := ParseIdentityEnvelope(value)
		if err != nil {
			h.rejectIdentity(c, err)
			return
		}
		c.Set("verified_identity", identity)
		c.Next()
	}
}

// GetMe returns the caller's verified identity. It performs no authorization
// or inventory computation and accepts no target filter.
func (h *Handler) GetMe(c *gin.Context) {
	value, exists := c.Get("verified_identity")
	if !exists {
		h.rejectIdentity(c, errors.New("trusted identity is required"))
		return
	}
	identity, ok := value.(*VerifiedIdentity)
	if !ok || identity == nil {
		h.rejectIdentity(c, errors.New("trusted identity is invalid"))
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, IdentityResponse{
		Issuer:            identity.Claims.Issuer,
		Subject:           identity.Claims.Subject,
		Name:              identity.Claims.Name,
		GivenName:         identity.Claims.GivenName,
		FamilyName:        identity.Claims.FamilyName,
		PreferredUsername: identity.Claims.PreferredUsername,
		Email:             identity.Claims.Email,
		Groups:            identity.Claims.Groups,
		Organizations:     identity.Claims.Organizations,
	})
}

// RejectTrustedIdentityHeaders prevents direct callers from injecting the new
// trust surface into Authorino callbacks.
func (h *Handler) RejectTrustedIdentityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := validateTrustedIdentityHeaders(c.Request.Header); err != nil {
			h.rejectIdentity(c, err)
			return
		}
		c.Next()
	}
}

func validateOptionalIdentityHeaders(header http.Header) error {
	// HTTP permits repeated headers. Accepting more than one copy would make
	// the effective identity depend on proxy header-ordering behavior.
	values := header.Values(constant.HeaderIdentity)
	if len(values) > 1 {
		return errors.New("duplicate identity header")
	}
	// Header.Get cannot distinguish an absent header from one explicit empty
	// value. Treat the latter as malformed rather than allowing it to select
	// the legacy no-identity fallback.
	if len(values) == 1 && values[0] == "" {
		return errors.New("identity header is empty")
	}
	return nil
}

func validateTrustedIdentityHeaders(header http.Header) error {
	if len(header.Values(constant.HeaderIdentity)) > 0 {
		return errors.New("identity header is not accepted")
	}
	return nil
}

func (h *Handler) rejectIdentity(c *gin.Context, err error) {
	requestLogger := h.logger.WithContext(c.Request.Context())
	requestLogger.Warn("Rejected trusted identity transport",
		"error", err.Error(),
		"identity_header_bytes", len(c.Request.Header.Get(constant.HeaderIdentity)),
	)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error":             "unauthorized",
		"exceptionCode":     "AUTH_FAILURE",
		"error_description": "trusted identity is invalid",
	})
}
