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
// Cluster DNS names like postgres.postgres.svc.cluster.local yield the embedded
// namespace label ("postgres").
func postgresNamespaceFromHost(hostname, appNamespace string) string {
	if hostname == "" || hostname == "postgres" || !strings.Contains(hostname, ".") {
		return appNamespace
	}
	if !strings.HasSuffix(hostname, ".svc.cluster.local") && !strings.HasSuffix(hostname, ".svc") {
		return appNamespace
	}
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 || parts[1] == "" {
		return appNamespace
	}
	return parts[1]
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
	if !isInClusterPostgresHost(host) {
		return false, "", nil
	}
	return true, postgresNamespaceFromHost(host, appNamespace), nil
}
