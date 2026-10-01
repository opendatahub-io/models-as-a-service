package maas

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const gatewayIdentityTokenBytes = 32

// ensureGatewayIdentityToken returns the shared gateway identity token used to
// inject and validate X-MaaS-Gateway-Auth. When the secret does not exist it is
// created automatically in the infrastructure namespace.
//
// RBAC uses get + create on resourceNames=maas-gateway-identity only. Kubernetes
// does not scope list/watch by resourceNames, so get is used to read that single
// secret instance without listing other secrets in the namespace.
func ensureGatewayIdentityToken(ctx context.Context, c client.Client, namespace, configuredToken string) (string, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return "", errors.New("infrastructure namespace is required to ensure gateway identity secret")
	}

	secretName := types.NamespacedName{
		Namespace: namespace,
		Name:      tenantreconcile.MaaSGatewayIdentitySecretName,
	}

	existing := &corev1.Secret{}
	err := c.Get(ctx, secretName, existing)
	switch {
	case err == nil:
		token := strings.TrimSpace(string(existing.Data[tenantreconcile.MaaSGatewayIdentitySecretKey]))
		if token == "" {
			return "", fmt.Errorf("gateway identity secret %s exists but %q is empty",
				secretName, tenantreconcile.MaaSGatewayIdentitySecretKey)
		}
		return token, nil
	case apierrors.IsNotFound(err):
		token := strings.TrimSpace(configuredToken)
		if token == "" {
			var genErr error
			token, genErr = generateGatewayIdentityToken()
			if genErr != nil {
				return "", genErr
			}
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName.Name,
				Namespace: secretName.Namespace,
				Labels: map[string]string{
					"app":                          "maas-api",
					"app.kubernetes.io/managed-by": "maas-controller",
					"app.kubernetes.io/part-of":    "maas-gateway-auth",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				tenantreconcile.MaaSGatewayIdentitySecretKey: []byte(token),
			},
		}
		if err := c.Create(ctx, secret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ensureGatewayIdentityToken(ctx, c, namespace, configuredToken)
			}
			return "", fmt.Errorf("failed to create gateway identity secret %s: %w", secretName, err)
		}
		return token, nil
	default:
		return "", fmt.Errorf("failed to get gateway identity secret %s: %w", secretName, err)
	}
}

func generateGatewayIdentityToken() (string, error) {
	buf := make([]byte, gatewayIdentityTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate gateway identity token: %w", err)
	}
	// URL-safe base64 without padding; matches openssl-based deploy scripts.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
