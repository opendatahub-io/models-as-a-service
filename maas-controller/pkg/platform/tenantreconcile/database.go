package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// postgresHostFromConnectionURL returns the hostname from a PostgreSQL connection URL.
func postgresHostFromConnectionURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse connection URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("connection URL missing hostname")
	}
	return host, nil
}

// isInClusterPostgresHost reports whether the database hostname targets bundled
// in-cluster Postgres (short name "postgres" or a Kubernetes cluster DNS name).
func isInClusterPostgresHost(hostname string) bool {
	if hostname == "postgres" {
		return true
	}
	return strings.HasSuffix(hostname, ".svc.cluster.local") || strings.HasSuffix(hostname, ".svc")
}

// postgresNamespaceFromHost returns the Kubernetes namespace for an in-cluster
// Postgres hostname. Short name "postgres" (or any non-FQDN) maps to appNamespace.
// Cluster DNS names use the label immediately before ".svc" so both Service
// forms (postgres.<ns>.svc…) and pod-qualified forms
// (postgres-0.postgres.<ns>.svc…) resolve correctly.
func postgresNamespaceFromHost(hostname, appNamespace string) string {
	if hostname == "" || hostname == "postgres" || !strings.Contains(hostname, ".") {
		return appNamespace
	}
	if !strings.HasSuffix(hostname, ".svc.cluster.local") && !strings.HasSuffix(hostname, ".svc") {
		return appNamespace
	}
	parts := strings.Split(hostname, ".")
	for i, part := range parts {
		if part == "svc" && i > 0 && parts[i-1] != "" {
			return parts[i-1]
		}
	}
	return appNamespace
}

// resolveBundledPostgres reads maas-db-config and returns whether the connection
// URL targets in-cluster Postgres plus that Postgres namespace. External databases
// (RDS, etc.) return false so maas-api-egress-restrict omits the app=postgres peer;
// administrators apply a companion egress policy with ipBlock CIDRs instead.
func resolveBundledPostgres(ctx context.Context, c client.Reader, appNamespace string) (bool, string, error) {
	if appNamespace == "" {
		return false, "", nil
	}

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: appNamespace, Name: MaaSDBSecretName}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("read %s secret: %w", MaaSDBSecretName, err)
	}

	rawURL := string(secret.Data[MaaSDBSecretKey])
	if rawURL == "" {
		return false, "", nil
	}

	host, err := postgresHostFromConnectionURL(rawURL)
	if err != nil {
		return false, "", err
	}
	if isInClusterPostgresHost(host) {
		return true, postgresNamespaceFromHost(host, appNamespace), nil
	}

	// postgres.<namespace> is a valid in-cluster Service hostname but is not
	// matched by isInClusterPostgresHost (no .svc suffix). Confirm the Service
	// exists with app=postgres before treating it as bundled; otherwise leave
	// it as external so egress does not open port 5432 to a mistaken peer.
	parts := strings.Split(host, ".")
	if len(parts) != 2 || parts[0] != "postgres" || parts[1] == "" {
		return false, "", nil
	}
	service := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Namespace: parts[1], Name: "postgres"}, service)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("read postgres service: %w", err)
	}
	if service.Spec.Selector["app"] != "postgres" {
		return false, "", nil
	}
	return true, parts[1], nil
}
