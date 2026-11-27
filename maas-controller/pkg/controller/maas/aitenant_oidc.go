/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"crypto/x509"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oidcprobe"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

const (
	// oidcTrustedCAConfigMapName is the ConfigMap in the tenant infra namespace
	// that carries the DSCInitialization trusted CA bundle. The probe builds
	// its trust pool from the same bundle the portal auth proxy consumes, but
	// the two are not identical observers: they run in different pods and
	// network paths, the probe honours HTTPS_PROXY, and the proxy receives the
	// issuer URL verbatim (the probe ignores a trailing slash). OIDCReady
	// reports issuer health, not proxy health.
	oidcTrustedCAConfigMapName = "odh-trusted-ca-bundle"
	// oidcRequeueInterval is how often an AITenant whose OIDCReady condition
	// is False is re-probed. Healthy tenants are not requeued: the requeue
	// re-runs the whole bootstrap reconcile, so it only pays while the issuer
	// is unhealthy. A healthy issuer that breaks later is noticed on the next
	// AITenant/watch-driven reconcile, not within a fixed interval.
	oidcRequeueInterval = 5 * time.Minute
	// oidcProbeMinInterval throttles probes triggered by watch events
	// (MaasTenantConfig/Gateway) so they do not hammer the issuer. The cache
	// is keyed by issuer, so tenants sharing an IdP share one probe.
	oidcProbeMinInterval = time.Minute
	// oidcProbeDeadline bounds the whole probe (discovery + JWKS), not each
	// request: the AITenant controller has a single worker, so a probe must
	// not hold it for the sum of the per-request timeouts.
	oidcProbeDeadline = 8 * time.Second
)

// oidcTrustedCAConfigMapKeys are the keys checked in oidcTrustedCAConfigMapName.
var oidcTrustedCAConfigMapKeys = []string{"ca-bundle.crt", "odh-ca-bundle.crt"} //nolint:gochecknoglobals // Constant key set.

// oidcProbeFunc probes an OIDC issuer; a field on the reconciler so tests can
// inject a fake instead of doing real HTTP.
type oidcProbeFunc func(ctx context.Context, issuerURL string, roots *x509.CertPool) oidcprobe.Result

// oidcProbeCacheEntry remembers the last probe for an issuer URL so
// watch-driven reconciles within oidcProbeMinInterval reuse it instead of
// re-probing. The cache is keyed by issuer (trailing-slash-insensitive), not
// by tenant: tenants sharing an identity provider share one probe, so one
// dead IdP cannot multiply into N probes holding the single AITenant worker.
type oidcProbeCacheEntry struct {
	at     time.Time
	result oidcprobe.Result
}

// oidcProbeCacheMaxEntries bounds the issuer-keyed probe cache: a tenant
// churn loop with distinct issuer URLs must not grow it without limit.
// Eviction keeps the most recent entries only.
const oidcProbeCacheMaxEntries = 128

// oidcConfigCacheEntry remembers the client-config evaluation for a tenant so
// watch-driven reconciles within oidcProbeMinInterval do not re-read the
// clientSecretRef Secret (an uncached API read) when nothing changed. The
// fingerprint covers the tenant namespace and the secret reference, so a spec
// change invalidates it immediately; secret content changes (a key appearing
// or disappearing) are picked up within the TTL, the same staleness bound the
// probe cache already accepts.
type oidcConfigCacheEntry struct {
	fingerprint string
	at          time.Time
	failed      bool
	reason      string
	message     string
}

// oidcProbeCacheKey normalizes an issuer URL for the probe cache: the probe
// itself tolerates a trailing slash, so both spellings must share one entry.
func oidcProbeCacheKey(issuerURL string) string {
	return strings.TrimRight(issuerURL, "/")
}

// oidcConfigured reports whether the AITenant has an OIDC issuer to probe.
func oidcConfigured(aitenant *maasv1alpha1.AITenant) bool {
	return aitenant.Spec.OIDC != nil && aitenant.Spec.OIDC.IssuerURL != ""
}

// reconcileOIDCReady sets, updates or removes the OIDCReady condition. It
// never fails: probe errors only surface through the condition. It must be
// called before updateAITenantStatus so the change lands in the same write.
// A transition to False also emits a warning event so `oc describe` surfaces
// it without reading conditions. A cancelled context (controller shutdown)
// skips the update entirely so a probe aborted mid-shutdown is not persisted
// as an issuer failure.
func (r *AITenantReconciler) reconcileOIDCReady(ctx context.Context, aitenant *maasv1alpha1.AITenant) {
	if !oidcConfigured(aitenant) {
		apimeta.RemoveStatusCondition(&aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
		return
	}
	if ctx.Err() != nil {
		return
	}

	previous := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	status, reason, message, detail := r.probeTenantOIDC(ctx, aitenant)
	if detail != "" {
		oteljson.FromContext(ctx).Info("OIDC issuer probe failed",
			"aitenant", aitenant.Namespace+"/"+aitenant.Name,
			"issuer", oidcProbeCacheKey(aitenant.Spec.OIDC.IssuerURL),
			"reason", reason, "detail", detail)
	}
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AITenantConditionOIDCReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: aitenant.Generation,
	})
	if status == metav1.ConditionFalse &&
		(previous == nil || previous.Status != metav1.ConditionFalse) &&
		r.Recorder != nil {
		r.Recorder.Eventf(aitenant, nil, corev1.EventTypeWarning, "OIDCNotReady", "Reconcile",
			"OIDC issuer for AITenant %s/%s is not ready: %s (%s)",
			aitenant.Namespace, aitenant.Name, reason, message)
	}
}

// probeTenantOIDC runs the client-config checks and (throttled) issuer probe,
// returning condition fields plus the raw probe detail for logging.
func (r *AITenantReconciler) probeTenantOIDC(ctx context.Context, aitenant *maasv1alpha1.AITenant) (metav1.ConditionStatus, string, string, string) {
	oidcCfg := aitenant.Spec.OIDC

	// Client config first: no point probing an issuer for an unusable client.
	if oidcCfg.ClientID == "" {
		return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete, "spec.oidc.clientId is empty", ""
	}
	if ref := oidcCfg.ClientSecretRef; ref != nil {
		tenantNS := r.tenantNamespaceName(aitenant)
		fingerprint := tenantNS + "/" + ref.Name + "/" + ref.Key
		// The Secret read goes through the APIReader (uncached), so reuse the
		// evaluation within the throttle window instead of paying it on every
		// watch-driven reconcile.
		if cached, ok := r.cachedOIDCConfig(aitenant, fingerprint); ok {
			if cached.failed {
				return metav1.ConditionFalse, cached.reason, cached.message, ""
			}
		} else {
			secret := &corev1.Secret{}
			err := r.get(ctx, types.NamespacedName{Namespace: tenantNS, Name: ref.Name}, secret)
			switch {
			case apierrors.IsNotFound(err):
				r.rememberOIDCConfig(aitenant, fingerprint, true,
					oidcprobe.ReasonClientConfigIncomplete,
					fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s not found", tenantNS, ref.Name))
				return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
					fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s not found", tenantNS, ref.Name), ""
			case err != nil:
				// The raw API error is deliberately left out of the condition:
				// condition messages must stay deterministic (see oidcprobe.Result).
				oteljson.FromContext(ctx).Info("cannot read spec.oidc.clientSecretRef secret",
					"namespace", tenantNS, "name", ref.Name, "detail", err)
				r.rememberOIDCConfig(aitenant, fingerprint, true,
					oidcprobe.ReasonClientConfigIncomplete,
					fmt.Sprintf("cannot read spec.oidc.clientSecretRef secret %s/%s", tenantNS, ref.Name))
				return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
					fmt.Sprintf("cannot read spec.oidc.clientSecretRef secret %s/%s", tenantNS, ref.Name), ""
			default:
				// Presence check only; the secret value is never read, logged
				// or copied.
				if _, ok := secret.Data[ref.Key]; !ok {
					r.rememberOIDCConfig(aitenant, fingerprint, true,
						oidcprobe.ReasonClientConfigIncomplete,
						fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s has no key %s", tenantNS, ref.Name, ref.Key))
					return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
						fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s has no key %s", tenantNS, ref.Name, ref.Key), ""
				}
				r.rememberOIDCConfig(aitenant, fingerprint, false, "", "")
			}
		}
	}

	if result, ok := r.cachedOIDCProbe(oidcCfg.IssuerURL); ok {
		return oidcResultToCondition(result)
	}

	probe := r.OIDCProbe
	if probe == nil {
		probe = oidcprobe.Probe
	}
	// One deadline for the whole probe: the AITenant controller runs a single
	// worker, and the probe must not hold it for the sum of the per-request
	// timeouts.
	probeCtx, cancel := context.WithTimeout(ctx, oidcProbeDeadline)
	defer cancel()
	result := probe(probeCtx, oidcCfg.IssuerURL, r.oidcCAPool(ctx, aitenant))
	r.rememberOIDCProbe(oidcCfg.IssuerURL, result)
	return oidcResultToCondition(result)
}

func oidcResultToCondition(result oidcprobe.Result) (metav1.ConditionStatus, string, string, string) {
	status := metav1.ConditionFalse
	if result.OK {
		status = metav1.ConditionTrue
	}
	return status, result.Reason, result.Message, result.Detail
}

// cachedOIDCProbe returns the previous result when the same issuer URL was
// probed less than oidcProbeMinInterval ago, by any tenant.
func (r *AITenantReconciler) cachedOIDCProbe(issuerURL string) (oidcprobe.Result, bool) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	entry, ok := r.oidcProbeLast[oidcProbeCacheKey(issuerURL)]
	if !ok || time.Since(entry.at) >= oidcProbeMinInterval {
		return oidcprobe.Result{}, false
	}
	return entry.result, true
}

func (r *AITenantReconciler) rememberOIDCProbe(issuerURL string, result oidcprobe.Result) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	if r.oidcProbeLast == nil {
		r.oidcProbeLast = map[string]oidcProbeCacheEntry{}
	}
	if len(r.oidcProbeLast) >= oidcProbeCacheMaxEntries {
		// Keep the most recent half: a churn loop with distinct issuers must
		// not grow the map without limit.
		r.evictStaleOIDCProbesLocked(time.Now().Add(-oidcProbeMinInterval))
		if len(r.oidcProbeLast) >= oidcProbeCacheMaxEntries {
			r.evictOldestOIDCProbesLocked(len(r.oidcProbeLast) / 2)
		}
	}
	r.oidcProbeLast[oidcProbeCacheKey(issuerURL)] = oidcProbeCacheEntry{
		at:     time.Now(),
		result: result,
	}
}

func (r *AITenantReconciler) evictStaleOIDCProbesLocked(cutoff time.Time) {
	for issuer, entry := range r.oidcProbeLast {
		if entry.at.Before(cutoff) {
			delete(r.oidcProbeLast, issuer)
		}
	}
}

func (r *AITenantReconciler) evictOldestOIDCProbesLocked(keep int) {
	type aged struct {
		issuer string
		at     time.Time
	}
	entries := make([]aged, 0, len(r.oidcProbeLast))
	for issuer, entry := range r.oidcProbeLast {
		entries = append(entries, aged{issuer, entry.at})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
	for _, entry := range entries[keep:] {
		delete(r.oidcProbeLast, entry.issuer)
	}
}

// cachedOIDCConfig returns the remembered client-config evaluation for the
// tenant when the fingerprint matches and it is within the throttle window.
func (r *AITenantReconciler) cachedOIDCConfig(aitenant *maasv1alpha1.AITenant, fingerprint string) (oidcConfigCacheEntry, bool) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	entry, ok := r.oidcConfigLast[client.ObjectKeyFromObject(aitenant)]
	if !ok || entry.fingerprint != fingerprint || time.Since(entry.at) >= oidcProbeMinInterval {
		return oidcConfigCacheEntry{}, false
	}
	return entry, true
}

func (r *AITenantReconciler) rememberOIDCConfig(aitenant *maasv1alpha1.AITenant, fingerprint string, failed bool, reason, message string) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	if r.oidcConfigLast == nil {
		r.oidcConfigLast = map[types.NamespacedName]oidcConfigCacheEntry{}
	}
	r.oidcConfigLast[client.ObjectKeyFromObject(aitenant)] = oidcConfigCacheEntry{
		fingerprint: fingerprint,
		at:          time.Now(),
		failed:      failed,
		reason:      reason,
		message:     message,
	}
}

// oidcCAPool returns the system roots plus the trusted CA bundle from the
// tenant infra namespace, mirroring the bundle source the portal auth proxy
// consumes. Missing ConfigMap or keys fall back to system roots only. The
// ConfigMap is read through the client each probe, so CA fixes apply without
// a restart. Note the probe and the proxy are still different observers (see
// oidcTrustedCAConfigMapName): OIDCReady=True does not imply the proxy is
// healthy.
func (r *AITenantReconciler) oidcCAPool(ctx context.Context, aitenant *maasv1alpha1.AITenant) *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	ns := r.oidcInfraNamespace(ctx, aitenant)
	if ns == "" {
		return pool
	}
	caBundle := &corev1.ConfigMap{}
	if err := r.get(ctx, types.NamespacedName{Namespace: ns, Name: oidcTrustedCAConfigMapName}, caBundle); err != nil {
		// Missing or unreadable bundle is not an error: system roots only.
		return pool
	}
	for _, key := range oidcTrustedCAConfigMapKeys {
		if pem := caBundle.Data[key]; pem != "" {
			pool.AppendCertsFromPEM([]byte(pem))
		}
	}
	return pool
}

// oidcInfraNamespace resolves the tenant infra namespace from the
// MaasTenantConfig status, falling back to the controller's AppNamespace.
func (r *AITenantReconciler) oidcInfraNamespace(ctx context.Context, aitenant *maasv1alpha1.AITenant) string {
	config := &maasv1alpha1.MaasTenantConfig{}
	key := types.NamespacedName{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: r.tenantNamespaceName(aitenant),
	}
	if err := r.get(ctx, key, config); err == nil && config.Status.InfraNamespace != "" {
		return config.Status.InfraNamespace
	}
	return r.AppNamespace
}

// oidcReadyIsFalse reports whether the AITenant's OIDCReady condition is
// currently False, i.e. whether the issuer needs re-probing on a timer.
func oidcReadyIsFalse(aitenant *maasv1alpha1.AITenant) bool {
	condition := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	return condition != nil && condition.Status == metav1.ConditionFalse
}
