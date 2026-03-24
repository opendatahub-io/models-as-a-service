/*
Copyright 2025.

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
	"fmt"

	"github.com/go-logr/logr"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	maasAPIAuthPolicyName     = "maas-api-auth-policy"
	maasAPIRouteName          = "maas-api-route"
	maasPlatformAuthFinalizer = "maas.opendatahub.io/platform-auth-cleanup"
	odhManagedAnnotation      = "opendatahub.io/managed"
)

// MaaSPlatformAuthReconciler reconciles a MaaSPlatformAuth object.
//
// The base maas-api-auth-policy AuthPolicy is deployed by kustomize. When a
// MaaSPlatformAuth CR exists, this controller adds opendatahub.io/managed=false
// to prevent the ODH operator from overwriting its changes, then updates the
// AuthPolicy to add external OIDC rules. On CR deletion, OIDC rules and the
// managed annotation are removed so the operator can manage the resource again.
type MaaSPlatformAuthReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MaaSAPINamespace is the namespace where maas-api is deployed.
	MaaSAPINamespace string

	// ClusterAudience is the OIDC audience for TokenReview.
	ClusterAudience string

	// GatewayName is the name of the Gateway used for model HTTPRoutes.
	GatewayName string
}

func (r *MaaSPlatformAuthReconciler) clusterAudience() string {
	if r.ClusterAudience != "" {
		return r.ClusterAudience
	}
	return defaultClusterAudience
}

func (r *MaaSPlatformAuthReconciler) gatewayAudience() string {
	gw := r.GatewayName
	if gw == "" {
		gw = defaultGatewayName
	}
	return gw + "-sa"
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasplatformauths,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasplatformauths/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasplatformauths/finalizers,verbs=update
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *MaaSPlatformAuthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSPlatformAuth", req.NamespacedName)

	platformAuth := &maasv1alpha1.MaaSPlatformAuth{}
	if err := r.Get(ctx, req.NamespacedName, platformAuth); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSPlatformAuth")
		return ctrl.Result{}, err
	}

	if !platformAuth.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, platformAuth)
	}

	if !controllerutil.ContainsFinalizer(platformAuth, maasPlatformAuthFinalizer) {
		controllerutil.AddFinalizer(platformAuth, maasPlatformAuthFinalizer)
		if err := r.Update(ctx, platformAuth); err != nil {
			return ctrl.Result{}, err
		}
	}

	statusSnapshot := platformAuth.Status.DeepCopy()

	if err := r.reconcileAuthPolicy(ctx, log, platformAuth.Spec.ExternalOIDC, true); err != nil {
		log.Error(err, "failed to reconcile maas-api AuthPolicy")
		_ = r.updatePlatformAuthStatus(ctx, platformAuth, "Failed", fmt.Sprintf("Failed to reconcile: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	if err := r.updatePlatformAuthStatus(ctx, platformAuth, "Active", "Successfully reconciled", statusSnapshot); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileAuthPolicy updates the existing maas-api-auth-policy with the full
// rule set. The AuthPolicy is created by kustomize; this controller only
// updates its spec.rules.
//
// When takeOwnership is true the controller adds opendatahub.io/managed=false
// so the ODH operator does not overwrite the OIDC-enriched rules. When false
// (deletion path) the annotation is removed so the operator can manage the
// resource again.
func (r *MaaSPlatformAuthReconciler) reconcileAuthPolicy(ctx context.Context, log logr.Logger, oidc *maasv1alpha1.ExternalOIDCConfig, takeOwnership bool) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(authPolicyGVK())
	if err := r.Get(ctx, types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: r.MaaSAPINamespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("maas-api AuthPolicy not found yet (kustomize may not have deployed it), will retry")
		}
		return fmt.Errorf("failed to get maas-api AuthPolicy: %w", err)
	}

	snapshot := existing.DeepCopy()

	annotations := existing.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	if takeOwnership {
		annotations[odhManagedAnnotation] = "false"
	} else {
		delete(annotations, odhManagedAnnotation)
	}
	existing.SetAnnotations(annotations)

	rules := r.buildAuthPolicyRules(oidc)
	if err := unstructured.SetNestedMap(existing.Object, rules, "spec", "rules"); err != nil {
		return fmt.Errorf("failed to set spec.rules: %w", err)
	}

	if equality.Semantic.DeepEqual(snapshot.Object, existing.Object) {
		log.Info("maas-api AuthPolicy unchanged, skipping update")
		return nil
	}

	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update maas-api AuthPolicy: %w", err)
	}
	log.Info("maas-api AuthPolicy updated", "oidcEnabled", oidc != nil, "managedByController", takeOwnership)
	return nil
}

// handleDeletion restores the base AuthPolicy (no OIDC), removes the
// opendatahub.io/managed annotation, then removes the finalizer.
func (r *MaaSPlatformAuthReconciler) handleDeletion(ctx context.Context, log logr.Logger, platformAuth *maasv1alpha1.MaaSPlatformAuth) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(platformAuth, maasPlatformAuthFinalizer) {
		if err := r.reconcileAuthPolicy(ctx, log, nil, false); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "failed to restore base AuthPolicy, will retry")
				return ctrl.Result{}, err
			}
			log.Info("AuthPolicy not found during deletion (namespace may be tearing down), proceeding with finalizer removal")
		}

		controllerutil.RemoveFinalizer(platformAuth, maasPlatformAuthFinalizer)
		if err := r.Update(ctx, platformAuth); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// buildAuthPolicyRules constructs the complete rules map for maas-api-auth-policy.
// When oidc is nil, only API key and OpenShift TokenReview authentication are included.
// When oidc is set, external OIDC JWT validation and client binding are added.
func (r *MaaSPlatformAuthReconciler) buildAuthPolicyRules(oidc *maasv1alpha1.ExternalOIDCConfig) map[string]interface{} {
	return map[string]interface{}{
		"authentication": r.buildAuthentication(oidc),
		"metadata":       r.buildMetadata(),
		"authorization":  r.buildAuthorization(oidc),
		"response":       r.buildResponse(oidc),
	}
}

func (r *MaaSPlatformAuthReconciler) buildAuthentication(oidc *maasv1alpha1.ExternalOIDCConfig) map[string]interface{} {
	openshiftPriority := int64(1)
	if oidc != nil {
		openshiftPriority = int64(2)
	}

	auth := map[string]interface{}{
		"api-keys": map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{"predicate": `request.headers.authorization.startsWith("Bearer sk-oai-")`},
			},
			"plain": map[string]interface{}{
				"selector": "request.headers.authorization",
			},
			"priority": int64(0),
		},
		"openshift-identities": map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`},
			},
			"kubernetesTokenReview": map[string]interface{}{
				"audiences": []interface{}{r.clusterAudience(), r.gatewayAudience()},
			},
			"priority": openshiftPriority,
		},
	}

	if oidc != nil {
		ttl := int64(oidc.TTL)
		if ttl == 0 {
			ttl = 300
		}
		auth["oidc-identities"] = map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
				},
			},
			"jwt": map[string]interface{}{
				"issuerUrl": oidc.IssuerURL,
				"ttl":       ttl,
			},
			"priority": int64(1),
		}
	}

	return auth
}

func (r *MaaSPlatformAuthReconciler) buildMetadata() map[string]interface{} {
	apiKeyValidationURL := fmt.Sprintf(
		"https://maas-api.%s.svc.cluster.local:8443/internal/v1/api-keys/validate",
		r.MaaSAPINamespace,
	)
	return map[string]interface{}{
		"apiKeyValidation": map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{"predicate": `request.headers.authorization.startsWith("Bearer sk-oai-")`},
			},
			"http": map[string]interface{}{
				"url":         apiKeyValidationURL,
				"method":      "POST",
				"contentType": "application/json",
				"body": map[string]interface{}{
					"expression": `{"key": request.headers.authorization.replace("Bearer ", "")}`,
				},
			},
			"priority": int64(0),
		},
	}
}

func (r *MaaSPlatformAuthReconciler) buildAuthorization(oidc *maasv1alpha1.ExternalOIDCConfig) map[string]interface{} {
	authz := map[string]interface{}{
		"api-key-valid": map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{"predicate": `request.headers.authorization.startsWith("Bearer sk-oai-")`},
			},
			"patternMatching": map[string]interface{}{
				"patterns": []interface{}{
					map[string]interface{}{
						"selector": "auth.metadata.apiKeyValidation.valid",
						"operator": "eq",
						"value":    "true",
					},
				},
			},
			"priority": int64(0),
		},
	}

	if oidc != nil {
		authz["oidc-client-bound"] = map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
				},
			},
			"patternMatching": map[string]interface{}{
				"patterns": []interface{}{
					map[string]interface{}{
						"selector": "auth.identity.azp",
						"operator": "eq",
						"value":    oidc.ClientID,
					},
				},
			},
			"priority": int64(1),
		}
	}

	return authz
}

func (r *MaaSPlatformAuthReconciler) buildResponse(oidc *maasv1alpha1.ExternalOIDCConfig) map[string]interface{} {
	groupsOCExpr := `'["' + auth.identity.user.groups.join('","') + '"]'`
	if oidc != nil {
		groupsOCExpr = `has(auth.identity.groups) ? (size(auth.identity.groups) > 0 ? '["system:authenticated","' + auth.identity.groups.join('","') + '"]' : '["system:authenticated"]') : '["' + auth.identity.user.groups.join('","') + '"]'`
	}

	usernameOC := map[string]interface{}{
		"when": []interface{}{
			map[string]interface{}{"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`},
		},
		"plain": map[string]interface{}{
			"selector": "auth.identity.user.username",
		},
		"key":      "X-MaaS-Username",
		"priority": int64(1),
	}
	if oidc != nil {
		usernameOC = map[string]interface{}{
			"when": []interface{}{
				map[string]interface{}{"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`},
			},
			"plain": map[string]interface{}{
				"expression": `has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username)`,
			},
			"key":      "X-MaaS-Username",
			"priority": int64(1),
		}
	}

	return map[string]interface{}{
		"success": map[string]interface{}{
			"headers": map[string]interface{}{
				"X-MaaS-Username": map[string]interface{}{
					"when": []interface{}{
						map[string]interface{}{"predicate": `request.headers.authorization.startsWith("Bearer sk-oai-")`},
					},
					"plain": map[string]interface{}{
						"selector": "auth.metadata.apiKeyValidation.username",
					},
					"priority": int64(0),
				},
				"X-MaaS-Username-OC": usernameOC,
				"X-MaaS-Group": map[string]interface{}{
					"when": []interface{}{
						map[string]interface{}{"predicate": `request.headers.authorization.startsWith("Bearer sk-oai-")`},
					},
					"plain": map[string]interface{}{
						"expression": `size(auth.metadata.apiKeyValidation.groups) > 0 ? '["' + auth.metadata.apiKeyValidation.groups.join('","') + '"]' : '[]'`,
					},
					"priority": int64(0),
				},
				"X-MaaS-Group-OC": map[string]interface{}{
					"when": []interface{}{
						map[string]interface{}{"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`},
					},
					"plain": map[string]interface{}{
						"expression": groupsOCExpr,
					},
					"key":      "X-MaaS-Group",
					"priority": int64(1),
				},
			},
		},
	}
}

func (r *MaaSPlatformAuthReconciler) updatePlatformAuthStatus(ctx context.Context, platformAuth *maasv1alpha1.MaaSPlatformAuth, phase, message string, statusSnapshot *maasv1alpha1.MaaSPlatformAuthStatus) error {
	platformAuth.Status.Phase = phase
	platformAuth.Status.AuthPolicyName = maasAPIAuthPolicyName

	status := metav1.ConditionTrue
	reason := "Reconciled"
	if phase == "Failed" {
		status = metav1.ConditionFalse
		reason = "ReconcileFailed"
	}

	apimeta.SetStatusCondition(&platformAuth.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: platformAuth.GetGeneration(),
	})

	if equality.Semantic.DeepEqual(*statusSnapshot, platformAuth.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, platformAuth); err != nil {
		return fmt.Errorf("failed to update MaaSPlatformAuth status: %w", err)
	}
	return nil
}

func (r *MaaSPlatformAuthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	managedAuthPolicy := &unstructured.Unstructured{}
	managedAuthPolicy.SetGroupVersionKind(authPolicyGVK())

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSPlatformAuth{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.Funcs{UpdateFunc: deletionTimestampSet},
		))).
		Watches(managedAuthPolicy, handler.EnqueueRequestsFromMapFunc(
			r.mapManagedAuthPolicyToOwner,
		)).
		Complete(r)
}

// mapManagedAuthPolicyToOwner triggers reconciliation of MaaSPlatformAuth CRs
// when the maas-api-auth-policy is modified (e.g. by someone manually editing it).
func (r *MaaSPlatformAuthReconciler) mapManagedAuthPolicyToOwner(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != maasAPIAuthPolicyName {
		return nil
	}
	var list maasv1alpha1.MaaSPlatformAuthList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, pa := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: pa.Name, Namespace: pa.Namespace},
		})
	}
	return requests
}

func authPolicyGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}
}
