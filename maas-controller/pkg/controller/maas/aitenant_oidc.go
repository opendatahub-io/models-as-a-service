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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oidcprobe"
)

const (
	// oidcTrustedCAConfigMapName is the ConfigMap in the tenant infra namespace
	// that carries the DSCInitialization trusted CA bundle. The probe trusts
	// exactly what the portal auth proxy trusts.
	oidcTrustedCAConfigMapName = "odh-trusted-ca-bundle"
	// oidcRequeueInterval is how often a healthy AITenant with OIDC configured
	// is re-probed. It also picks up trusted-CA fixes without editing the AITenant.
	oidcRequeueInterval = 5 * time.Minute
	// oidcProbeMinInterval throttles probes triggered by watch events
	// (MaasTenantConfig/Gateway) so they do not hammer the issuer.
	oidcProbeMinInterval = time.Minute
)

// oidcTrustedCAConfigMapKeys are the keys checked in oidcTrustedCAConfigMapName.
var oidcTrustedCAConfigMapKeys = []string{"ca-bundle.crt", "odh-ca-bundle.crt"} //nolint:gochecknoglobals // Constant key set.

// oidcProbeFunc probes an OIDC issuer; a field on the reconciler so tests can
// inject a fake instead of doing real HTTP.
type oidcProbeFunc func(ctx context.Context, issuerURL string, roots *x509.CertPool) oidcprobe.Result

// oidcProbeCacheEntry remembers the last probe for an AITenant so watch-driven
// reconciles within oidcProbeMinInterval reuse it instead of re-probing. The
// AITenant UID is part of the identity: a deleted and recreated AITenant with
// the same name, issuer and generation must not inherit the old object's
// cached result.
type oidcProbeCacheEntry struct {
	uid        types.UID
	issuerURL  string
	generation int64
	at         time.Time
	result     oidcprobe.Result
}

// oidcConfigured reports whether the AITenant has an OIDC issuer to probe.
func oidcConfigured(aitenant *maasv1alpha1.AITenant) bool {
	return aitenant.Spec.OIDC != nil && aitenant.Spec.OIDC.IssuerURL != ""
}

// reconcileOIDCReady sets, updates or removes the OIDCReady condition. It
// never fails: probe errors only surface through the condition. It must be
// called before updateAITenantStatus so the change lands in the same write.
func (r *AITenantReconciler) reconcileOIDCReady(ctx context.Context, aitenant *maasv1alpha1.AITenant) {
	if !oidcConfigured(aitenant) {
		apimeta.RemoveStatusCondition(&aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
		return
	}

	status, reason, message := r.probeTenantOIDC(ctx, aitenant)
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AITenantConditionOIDCReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: aitenant.Generation,
	})
}

// probeTenantOIDC runs the client-config checks and (throttled) issuer probe,
// returning condition fields.
func (r *AITenantReconciler) probeTenantOIDC(ctx context.Context, aitenant *maasv1alpha1.AITenant) (metav1.ConditionStatus, string, string) {
	oidcCfg := aitenant.Spec.OIDC

	// Client config first: no point probing an issuer for an unusable client.
	if oidcCfg.ClientID == "" {
		return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete, "spec.oidc.clientId is empty"
	}
	if ref := oidcCfg.ClientSecretRef; ref != nil {
		tenantNS := r.tenantNamespaceName(aitenant)
		secret := &corev1.Secret{}
		err := r.get(ctx, types.NamespacedName{Namespace: tenantNS, Name: ref.Name}, secret)
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
				fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s not found", tenantNS, ref.Name)
		}
		if err != nil {
			return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
				fmt.Sprintf("cannot read spec.oidc.clientSecretRef secret %s/%s: %s", tenantNS, ref.Name, err)
		}
		// Presence check only; the secret value is never read, logged or copied.
		if _, ok := secret.Data[ref.Key]; !ok {
			return metav1.ConditionFalse, oidcprobe.ReasonClientConfigIncomplete,
				fmt.Sprintf("spec.oidc.clientSecretRef secret %s/%s has no key %s", tenantNS, ref.Name, ref.Key)
		}
	}

	if result, ok := r.cachedOIDCProbe(aitenant); ok {
		return oidcResultToCondition(result)
	}

	probe := r.OIDCProbe
	if probe == nil {
		probe = oidcprobe.Probe
	}
	result := probe(ctx, oidcCfg.IssuerURL, r.oidcCAPool(ctx, aitenant))
	r.rememberOIDCProbe(aitenant, result)
	return oidcResultToCondition(result)
}

func oidcResultToCondition(result oidcprobe.Result) (metav1.ConditionStatus, string, string) {
	status := metav1.ConditionFalse
	if result.OK {
		status = metav1.ConditionTrue
	}
	return status, result.Reason, result.Message
}

// cachedOIDCProbe returns the previous result when the same issuer URL was
// probed for the same generation less than oidcProbeMinInterval ago.
func (r *AITenantReconciler) cachedOIDCProbe(aitenant *maasv1alpha1.AITenant) (oidcprobe.Result, bool) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	entry, ok := r.oidcProbeLast[client.ObjectKeyFromObject(aitenant)]
	if !ok || entry.uid != aitenant.UID || entry.issuerURL != aitenant.Spec.OIDC.IssuerURL ||
		entry.generation != aitenant.Generation ||
		time.Since(entry.at) >= oidcProbeMinInterval {
		return oidcprobe.Result{}, false
	}
	return entry.result, true
}

func (r *AITenantReconciler) rememberOIDCProbe(aitenant *maasv1alpha1.AITenant, result oidcprobe.Result) {
	r.oidcProbeMu.Lock()
	defer r.oidcProbeMu.Unlock()
	if r.oidcProbeLast == nil {
		r.oidcProbeLast = map[types.NamespacedName]oidcProbeCacheEntry{}
	}
	r.oidcProbeLast[client.ObjectKeyFromObject(aitenant)] = oidcProbeCacheEntry{
		uid:        aitenant.UID,
		issuerURL:  aitenant.Spec.OIDC.IssuerURL,
		generation: aitenant.Generation,
		at:         time.Now(),
		result:     result,
	}
}

// oidcCAPool returns the system roots plus the trusted CA bundle from the
// tenant infra namespace (where the portal auth proxy runs), so the probe
// trusts exactly what the proxy trusts. Missing ConfigMap or keys fall back
// to system roots only. The ConfigMap is read through the client each probe,
// so CA fixes apply without a restart.
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
