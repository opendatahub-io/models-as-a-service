package token

import (
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // SHA1 used for non-cryptographic hashing of usernames, not for security
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corelistersv1 "k8s.io/client-go/listers/core/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tier"
)

type Manager struct {
	tenantName           string
	tierMapper           *tier.Mapper
	clientset            kubernetes.Interface
	namespaceLister      corelistersv1.NamespaceLister
	serviceAccountLister corelistersv1.ServiceAccountLister
	logger               *logger.Logger
}

func NewManager(
	log *logger.Logger,
	tenantName string,
	tierMapper *tier.Mapper,
	clientset kubernetes.Interface,
	namespaceLister corelistersv1.NamespaceLister,
	serviceAccountLister corelistersv1.ServiceAccountLister,
) *Manager {
	return &Manager{
		tenantName:           tenantName,
		tierMapper:           tierMapper,
		clientset:            clientset,
		namespaceLister:      namespaceLister,
		serviceAccountLister: serviceAccountLister,
		logger:               log,
	}
}

// GenerateToken creates a Service Account token in the namespace bound to the tier the user belongs to.
func (m *Manager) GenerateToken(ctx context.Context, user *UserContext, expiration time.Duration) (*Token, error) {
	log := m.logger.WithFields(
		"expiration", expiration.String(),
	)

	userTier, err := m.tierMapper.GetTierForGroups(user.Groups...)
	if err != nil {
		return nil, fmt.Errorf("failed to determine user tier for %s (groups: %v): %w", user.Username, user.Groups, err)
	}

	log = log.WithFields("tier", userTier.Name)
	log.Debug("Determined user tier")

	namespace, errNs := m.ensureTierNamespace(ctx, userTier.Name)
	if errNs != nil {
		return nil, fmt.Errorf("failed to ensure tier namespace for tier %s: %w", userTier.Name, errNs)
	}

	saName, errSA := m.ensureServiceAccount(ctx, namespace, user.Username, userTier.Name)
	if errSA != nil {
		return nil, fmt.Errorf("failed to ensure service account for user %s in namespace %s: %w", user.Username, namespace, errSA)
	}

	token, errToken := m.createServiceAccountToken(ctx, namespace, saName, int(expiration.Seconds()))
	if errToken != nil {
		return nil, fmt.Errorf("failed to create token for service account %s in namespace %s: %w", saName, namespace, errToken)
	}

	claims, err := extractClaims(token.Status.Token)
	if err != nil {
		return nil, fmt.Errorf("failed to extract claims from new token: %w", err)
	}
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		// Fallback: cluster does not emit a jti claim (ServiceAccountTokenJTI feature gate disabled or K8s < 1.29).
		// Generate a stable identifier locally for API key metadata.
		var errJTI error
		jti, errJTI = generateLocalJTI()
		if errJTI != nil {
			return nil, fmt.Errorf("jti claim not found and fallback generation failed: %w", errJTI)
		}
	}

	// Extract iat (issued at) claim from JWT - required for K8s SA tokens
	iat, err := claims.GetIssuedAt()
	if err != nil {
		return nil, fmt.Errorf("failed to extract iat claim: %w", err)
	}
	if iat == nil {
		return nil, errors.New("token is missing required 'iat' claim")
	}
	issuedAt := iat.Unix()

	log.Debug("Successfully generated token",
		"expires_at", token.Status.ExpirationTimestamp.Unix(),
		"jti", jti,
	)

	result := &Token{
		Token:      token.Status.Token,
		Expiration: Duration{expiration},
		ExpiresAt:  token.Status.ExpirationTimestamp.Unix(),
		IssuedAt:   issuedAt,
		JTI:        jti,
	}

	return result, nil
}

// RevokeTokens revokes all tokens for a user by recreating their Service Account.
func (m *Manager) RevokeTokens(ctx context.Context, user *UserContext) error {
	log := m.logger

	userTier, err := m.tierMapper.GetTierForGroups(user.Groups...)
	if err != nil {
		return fmt.Errorf("failed to determine user tier for %s (groups: %v): %w", user.Username, user.Groups, err)
	}

	log = log.WithFields("tier", userTier.Name)
	namespace, errNS := m.tierMapper.Namespace(userTier.Name)
	if errNS != nil {
		return fmt.Errorf("failed to determine namespace for tier %s: %w", userTier.Name, errNS)
	}

	saName, errName := m.sanitizeServiceAccountName(user.Username)
	if errName != nil {
		return fmt.Errorf("failed to sanitize service account name for user %s: %w", user.Username, errName)
	}

	_, err = m.serviceAccountLister.ServiceAccounts(namespace).Get(saName)
	if apierrors.IsNotFound(err) {
		log.Debug("Service account not found, nothing to revoke")
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to check service account %s in namespace %s: %w", saName, namespace, err)
	}

	err = m.deleteServiceAccount(ctx, namespace, saName)
	if err != nil {
		return fmt.Errorf("failed to delete service account %s in namespace %s: %w", saName, namespace, err)
	}

	_, err = m.ensureServiceAccount(ctx, namespace, user.Username, userTier.Name)
	if err != nil {
		return fmt.Errorf("failed to recreate service account for user %s in namespace %s: %w", user.Username, namespace, err)
	}

	log.Debug("Successfully revoked all tokens for user")
	return nil
}

// ensureTierNamespace creates a tier-based namespace if it doesn't exist.
// It takes a tier name, formats it as {instance}-tier-{tier}, and returns the namespace name.
func (m *Manager) ensureTierNamespace(ctx context.Context, tier string) (string, error) {
	namespace, errNs := m.tierMapper.Namespace(tier)
	if errNs != nil {
		return "", fmt.Errorf("failed to determine namespace for tier %q: %w", tier, errNs)
	}

	_, err := m.namespaceLister.Get(namespace)
	if err == nil {
		return namespace, nil
	}

	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to check namespace %s: %w", namespace, err)
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: namespaceLabels(m.tenantName, tier),
		},
	}

	_, err = m.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return namespace, nil
		}
		return "", fmt.Errorf("failed to create namespace %s: %w", namespace, err)
	}

	m.logger.Info("Created tier namespace",
		"tier", tier,
	)
	return namespace, nil
}

// ensureServiceAccount creates a service account if it doesn't exist.
// It takes a raw username, sanitizes it for Kubernetes naming, and returns the sanitized name.
func (m *Manager) ensureServiceAccount(ctx context.Context, namespace, username, userTier string) (string, error) {
	saName, errName := m.sanitizeServiceAccountName(username)
	if errName != nil {
		return "", fmt.Errorf("failed to sanitize service account name for user %s: %w", username, errName)
	}

	_, err := m.serviceAccountLister.ServiceAccounts(namespace).Get(saName)
	if err == nil {
		return saName, nil
	}

	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to check service account %s in namespace %s: %w", saName, namespace, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: namespace,
			Labels:    serviceAccountLabels(m.tenantName, userTier),
		},
	}

	_, err = m.clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return saName, nil
		}
		return "", fmt.Errorf("failed to create service account %s in namespace %s: %w", saName, namespace, err)
	}

	m.logger.Debug("Created service account",
		"tier", userTier,
	)
	return saName, nil
}

// createServiceAccountToken creates a token for the service account using TokenRequest.
func (m *Manager) createServiceAccountToken(ctx context.Context, namespace, saName string, ttl int) (*authv1.TokenRequest, error) {
	expirationSeconds := int64(ttl)

	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
			Audiences:         []string{m.tenantName + "-sa"},
		},
	}

	result, err := m.clientset.CoreV1().ServiceAccounts(namespace).CreateToken(
		ctx, saName, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create token for service account %s: %w", saName, err)
	}

	return result, nil
}

// deleteServiceAccount deletes a service account.
func (m *Manager) deleteServiceAccount(ctx context.Context, namespace, saName string) error {
	err := m.clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, saName, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete service account %s in namespace %s: %w", saName, namespace, err)
	}

	m.logger.Debug("Deleted service account")
	return nil
}

// sanitizeServiceAccountName ensures the service account name follows Kubernetes naming conventions.
// While ideally usernames should be pre-validated, Kubernetes TokenReview can return usernames
// in various formats (OIDC emails, LDAP DNs, etc.) that need sanitization for use as SA names.
func (m *Manager) sanitizeServiceAccountName(username string) (string, error) {
	// Kubernetes ServiceAccount names must be valid DNS-1123 labels:
	// [a-z0-9-], 1-63 chars, start/end alphanumeric.
	name := strings.ToLower(username)

	// Replace any invalid runes with '-'
	reInvalid := regexp.MustCompile(`[^a-z0-9-]+`)
	name = reInvalid.ReplaceAllString(name, "-")

	// Collapse consecutive dashes
	reDash := regexp.MustCompile(`-+`)
	name = reDash.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "", fmt.Errorf("invalid username %q", username)
	}

	// Append a stable short hash to reduce collisions
	sum := sha1.Sum([]byte(username)) //nolint:gosec // SHA1 used for non-cryptographic hashing, not for security
	suffix := hex.EncodeToString(sum[:])[:8]

	// Ensure total length <= 63 including hyphen and suffix
	const maxLen = 63
	baseMax := maxLen - 1 - len(suffix)
	if len(name) > baseMax {
		name = name[:baseMax]
		name = strings.Trim(name, "-")
	}

	return name + "-" + suffix, nil
}

// generateLocalJTI generates a local JTI identifier when the cluster does not provide one.
// This is needed for clusters running Kubernetes < 1.29 or when ServiceAccountTokenJTI feature gate is disabled.
func generateLocalJTI() (string, error) {
	const size = 16
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes for JTI: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Audience returns the expected service account token audience for this MaaS instance.
func (m *Manager) Audience() string {
	return m.tenantName + "-sa"
}

// HasValidAudience checks if the given JWT token has the expected service account audience.
// Returns false if the token cannot be parsed or doesn't contain the expected audience.
func (m *Manager) HasValidAudience(tokenString string) bool {
	claims, err := extractClaims(tokenString)
	if err != nil {
		m.logger.Warn("Failed to extract claims from token", "error", err)
		return false
	}

	aud, err := claims.GetAudience()
	if err != nil {
		m.logger.Warn("Failed to get audience from token claims", "error", err)
		return false
	}

	expected := m.Audience()
	if slices.Contains(aud, expected) {
		return true
	}

	m.logger.Debug("Token audience mismatch", "expected", expected, "actual", aud)
	return false
}
