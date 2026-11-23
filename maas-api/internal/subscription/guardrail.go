package subscription

import (
	"context"
	"errors"
	"fmt"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	maasguardrail "github.com/opendatahub-io/models-as-a-service/maas-common/pkg/guardrail"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// defaultTenantConfigName must match maas-controller MaasTenantConfigInstanceName; the CRD enforces this name.
const defaultTenantConfigName = "default-tenant"

// ResourceGetter reads one cached resource by namespace and name.
type ResourceGetter interface {
	Get(namespace, name string) (*unstructured.Unstructured, error)
}

// GuardrailPlanResolver resolves the effective guardrail checks for a selected subscription/model pair.
type GuardrailPlanResolver interface {
	SelectedChecks(ctx context.Context, subscription, model *unstructured.Unstructured) (map[string]bool, error)
}

type guardrailPlanResolver struct {
	tenantNamespace string
	tenantName      string
	tenantGetter    ResourceGetter
	tenantConfig    ResourceGetter
	policyGetter    ResourceGetter
}

// NewGuardrailPlanResolver creates a resolver backed by informer-cache readers.
func NewGuardrailPlanResolver(
	tenantNamespace, tenantName string,
	tenantGetter, tenantConfig, policyGetter ResourceGetter,
) *guardrailPlanResolver {
	return &guardrailPlanResolver{
		tenantNamespace: tenantNamespace,
		tenantName:      tenantName,
		tenantGetter:    tenantGetter,
		tenantConfig:    tenantConfig,
		policyGetter:    policyGetter,
	}
}

func (r *guardrailPlanResolver) SelectedChecks(
	ctx context.Context,
	subscription, model *unstructured.Unstructured,
) (map[string]bool, error) {
	if r == nil || r.tenantGetter == nil || r.tenantConfig == nil {
		return nil, errors.New("guardrail resolver is not configured")
	}
	if subscription == nil || model == nil {
		return nil, errors.New("subscription and model are required")
	}

	tenant, err := r.tenantGetter.Get(r.tenantNamespace, r.tenantName)
	if err != nil {
		return nil, fmt.Errorf("get AITenant %s/%s: %w", r.tenantNamespace, r.tenantName, err)
	}
	targetNamespace, found, err := unstructured.NestedString(tenant.Object, "status", "tenantNamespace")
	if err != nil || !found || targetNamespace == "" {
		return nil, fmt.Errorf("AITenant %s/%s has no resolved tenant namespace", r.tenantNamespace, r.tenantName)
	}
	if subscription.GetNamespace() != targetNamespace {
		return nil, fmt.Errorf("subscription %s/%s is outside AITenant target namespace %s", subscription.GetNamespace(), subscription.GetName(), targetNamespace)
	}
	resolvedTenant, found, err := unstructured.NestedString(model.Object, "status", "resolvedTenantRef")
	if err != nil || !found || resolvedTenant != r.tenantName {
		return nil, fmt.Errorf("MaaSModelRef %s/%s is not resolved to AITenant %s", model.GetNamespace(), model.GetName(), r.tenantName)
	}

	tenantConfig, err := r.tenantConfig.Get(targetNamespace, defaultTenantConfigName)
	if err != nil {
		return nil, fmt.Errorf("get MaasTenantConfig %s/%s: %w", targetNamespace, defaultTenantConfigName, err)
	}

	modelEntry, err := modelEntrySelection(subscription, model.GetNamespace(), model.GetName())
	if err != nil {
		return nil, err
	}

	resolver := aiguardrail.NewAttachmentResolver(&unstructuredPolicyReader{getter: r.policyGetter})
	tenantAttachments, err := attachments(tenant)
	if err != nil {
		return nil, err
	}
	tenantConfigAttachments, err := attachments(tenantConfig)
	if err != nil {
		return nil, err
	}
	subscriptionAttachments, err := attachments(subscription)
	if err != nil {
		return nil, err
	}
	modelAttachments, err := attachments(model)
	if err != nil {
		return nil, err
	}

	_, plan, err := maasguardrail.ComposeSubscriptionModelPlan(
		ctx,
		resolver,
		targetNamespace,
		&maasguardrail.TenantSelection{ResourceRef: resourceRef(tenant), Guardrails: tenantAttachments},
		&maasguardrail.TenantConfigSelection{ResourceRef: resourceRef(tenantConfig), Guardrails: tenantConfigAttachments},
		&maasguardrail.SubscriptionSelection{
			ResourceRef: resourceRef(subscription),
			Guardrails:  subscriptionAttachments,
			ModelRefs:   []maasguardrail.ModelEntrySelection{modelEntry},
		},
		&maasguardrail.ModelSelection{ResourceRef: resourceRef(model), Guardrails: modelAttachments},
		model.GetNamespace(),
		model.GetName(),
	)
	if err != nil {
		return nil, err
	}

	selected := make(map[string]bool, len(plan.Checks))
	for _, check := range plan.Checks {
		selected[aiguardrail.CheckID(check.Policy.Namespace, check.Policy.Name, check.Check.Name)] = true
	}
	return selected, nil
}

func resourceRef(obj *unstructured.Unstructured) maasguardrail.ResourceRef {
	return maasguardrail.ResourceRef{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		UID:       obj.GetUID(),
	}
}

func attachments(obj *unstructured.Unstructured) ([]aiguardrail.Attachment, error) {
	items, found, err := unstructured.NestedSlice(obj.Object, "spec", "guardrails")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	out := make([]aiguardrail.Attachment, 0, len(items))
	for _, item := range items {
		attachment, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("guardrail attachment %d is invalid", len(out))
		}
		ref, ok := attachment["ref"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("guardrail attachment %d has no reference", len(out))
		}
		name, _ := ref["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("guardrail attachment %d has an empty reference", len(out))
		}
		selected := aiguardrail.Attachment{Name: name}
		if rawChecks, found := attachment["checks"]; found {
			checks, ok := rawChecks.([]any)
			if !ok {
				return nil, fmt.Errorf("guardrail attachment %d has invalid checks", len(out))
			}
			for _, check := range checks {
				if checkName, ok := check.(string); ok {
					selected.Checks = append(selected.Checks, checkName)
				} else {
					return nil, fmt.Errorf("guardrail attachment %d has an invalid check", len(out))
				}
			}
		}
		out = append(out, selected)
	}
	return out, nil
}

func modelEntrySelection(subscription *unstructured.Unstructured, namespace, name string) (maasguardrail.ModelEntrySelection, error) {
	items, found, err := unstructured.NestedSlice(subscription.Object, "spec", "modelRefs")
	if err != nil || !found {
		return maasguardrail.ModelEntrySelection{}, fmt.Errorf("subscription %s/%s has no model entries", subscription.GetNamespace(), subscription.GetName())
	}

	var match *maasguardrail.ModelEntrySelection
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entryNamespace, _ := entry["namespace"].(string)
		entryName, _ := entry["name"].(string)
		if entryNamespace != namespace || entryName != name {
			continue
		}
		if match != nil {
			return maasguardrail.ModelEntrySelection{}, fmt.Errorf(
				"subscription %s/%s has duplicate model entry %s/%s",
				subscription.GetNamespace(), subscription.GetName(), namespace, name,
			)
		}
		spec := map[string]any{}
		if rawGuardrails, ok := entry["guardrails"]; ok {
			spec["guardrails"] = rawGuardrails
		}
		entryObject := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		guardrails, err := attachments(entryObject)
		if err != nil {
			return maasguardrail.ModelEntrySelection{}, err
		}
		candidate := maasguardrail.ModelEntrySelection{Namespace: namespace, Name: name, Guardrails: guardrails}
		match = &candidate
	}
	if match == nil {
		return maasguardrail.ModelEntrySelection{}, fmt.Errorf("subscription %s/%s does not include model %s/%s", subscription.GetNamespace(), subscription.GetName(), namespace, name)
	}
	return *match, nil
}

type unstructuredPolicyReader struct {
	getter ResourceGetter
}

func (r *unstructuredPolicyReader) GetPolicy(_ context.Context, namespace, name string) (*aiguardrail.Policy, error) {
	if r.getter == nil {
		return nil, fmt.Errorf("policy %s/%s: %w: AIGuardrail API unavailable at startup", namespace, name, aiguardrail.ErrReadFailure)
	}
	obj, err := r.getter.Get(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("policy %s/%s: %w", namespace, name, aiguardrail.ErrPolicyNotFound)
		}
		return nil, fmt.Errorf("policy %s/%s: %w: %w", namespace, name, aiguardrail.ErrReadFailure, err)
	}

	checks, err := policyChecks(obj)
	if err != nil {
		return nil, fmt.Errorf("policy %s/%s: %w: %w", namespace, name, aiguardrail.ErrReadFailure, err)
	}
	conditions, err := policyConditions(obj)
	if err != nil {
		return nil, fmt.Errorf("policy %s/%s: %w: %w", namespace, name, aiguardrail.ErrReadFailure, err)
	}
	bindingRevision, _, err := unstructured.NestedString(obj.Object, "status", "bindingRevision")
	if err != nil {
		return nil, fmt.Errorf("policy %s/%s: %w: %w", namespace, name, aiguardrail.ErrReadFailure, err)
	}
	return &aiguardrail.Policy{
		Namespace:       obj.GetNamespace(),
		Name:            obj.GetName(),
		UID:             obj.GetUID(),
		Generation:      obj.GetGeneration(),
		BindingRevision: bindingRevision,
		Conditions:      conditions,
		Checks:          checks,
	}, nil
}

func policyChecks(obj *unstructured.Unstructured) ([]aiguardrail.Check, error) {
	items, found, err := unstructured.NestedSlice(obj.Object, "spec", "checks")
	if err != nil || !found {
		return nil, errors.New("spec.checks is missing")
	}
	checks := make([]aiguardrail.Check, 0, len(items))
	for index, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("spec.checks[%d] is invalid", index)
		}
		name, _ := entry["name"].(string)
		phases, ok := entry["phases"].([]any)
		if name == "" || !ok {
			return nil, fmt.Errorf("spec.checks[%d] is invalid", index)
		}
		check := aiguardrail.Check{Name: name, SpecIndex: index}
		for _, phase := range phases {
			value, ok := phase.(string)
			if !ok {
				return nil, fmt.Errorf("spec.checks[%d].phases is invalid", index)
			}
			check.Phases = append(check.Phases, aigatewayv1alpha1.GuardrailPhase(value))
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func policyConditions(obj *unstructured.Unstructured) ([]aiguardrail.PolicyCondition, error) {
	items, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	conditions := make([]aiguardrail.PolicyCondition, 0, len(items))
	for index, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("status.conditions[%d] is invalid", index)
		}
		conditionType, _ := entry["type"].(string)
		status, _ := entry["status"].(string)
		observedGeneration, found, err := unstructured.NestedInt64(entry, "observedGeneration")
		if err != nil || !found || conditionType == "" {
			return nil, fmt.Errorf("status.conditions[%d] is invalid", index)
		}
		conditions = append(conditions, aiguardrail.PolicyCondition{
			Type:               conditionType,
			Status:             status == "True",
			ObservedGeneration: observedGeneration,
		})
	}
	return conditions, nil
}
