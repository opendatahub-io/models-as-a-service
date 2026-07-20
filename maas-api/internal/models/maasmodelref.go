package models

import (
	"encoding/json"
	"net/url"

	"github.com/openai/openai-go/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/apis"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

const (
	maasGroup    = "maas.opendatahub.io"
	maasVersion  = "v1alpha1"
	maasResource = "maasmodelrefs"

	// kindExternalModel and kindLLMISvc are the two valid values of MaaSModelRef spec.modelRef.kind.
	// An empty kind defaults to kindLLMISvc ("llmisvc").
	kindExternalModel    = "ExternalModel"
	kindLLMISvc          = "llmisvc"
	kindLLMISvcAlternate = "LLMInferenceService"
)

// MaaSModelRefLister lists MaaSModelRef CRs from a cache (e.g. informer-backed). Used for GET /v1/models.
type MaaSModelRefLister interface {
	// List returns all MaaSModelRef unstructured items from all namespaces.
	List() ([]*unstructured.Unstructured, error)
}

// ListFromMaaSModelRefLister converts cached MaaSModelRef items to API models. Uses status.endpoint and status.phase.
func ListFromMaaSModelRefLister(lister MaaSModelRefLister) ([]Model, error) {
	if lister == nil {
		return nil, nil
	}
	items, err := lister.List()
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(items))
	for _, u := range items {
		m := maasModelRefToModel(u)
		if m != nil {
			out = append(out, *m)
		}
	}
	return out, nil
}

// GVR returns the GroupVersionResource for MaaSModelRef CRs.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: maasGroup, Version: maasVersion, Resource: maasResource}
}

// maasModelRefToModel converts a MaaSModelRef unstructured to a Model for the API.
//
// For LLMInferenceService-backed models (BBR clusters), the model ID is read from
// status.resolvedModelAlias (the canonical publishers/{ns}/models/{name} form), and
// the URL is derived from status.httpRouteHostnames[0] (the shared gateway base URL).
// For ExternalModel refs, the ExternalModel CR name is used as the ID.
func maasModelRefToModel(u *unstructured.Unstructured) *Model {
	if u == nil {
		return nil
	}
	name := u.GetName()
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	endpoint, _, _ := unstructured.NestedString(u.Object, "status", "endpoint")
	ready := phase == "Ready"
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "modelRef", "kind")
	if kind == "" {
		kind = kindLLMISvc
	}

	modelRefName, _, _ := unstructured.NestedString(u.Object, "spec", "modelRef", "name")

	modelID := name
	switch kind {
	case kindExternalModel:
		// For ExternalModel refs, use the ExternalModel CR name as the model ID so that
		// GET /v1/models returns the identifier that inference endpoints expect.
		// ExternalModels skip the backend probe (no /v1/models discovery), so without
		// this the catalog would expose the MaaSModelRef name which doesn't work for inference.
		if modelRefName != "" {
			modelID = modelRefName
		}
	default:
		// For LLMInferenceService-backed models, use status.resolvedModelAlias as the
		// canonical BBR model ID (publishers/{namespace}/models/{model-name}), which is
		// the correct identifier clients must use in the "model" field of inference requests.
		// Falls back to metadata.name when resolvedModelAlias is not yet populated.
		if alias, _, _ := unstructured.NestedString(u.Object, "status", "resolvedModelAlias"); alias != "" {
			modelID = alias
		}
	}

	annotations := u.GetAnnotations()
	var details *Details
	if annotations != nil {
		d := Details{
			DisplayName:   annotations[constant.AnnotationDisplayName],
			Description:   annotations[constant.AnnotationDescription],
			GenAIUseCase:  annotations[constant.AnnotationGenAIUseCase],
			ContextWindow: annotations[constant.AnnotationContextWindow],
		}
		if raw := annotations[constant.AnnotationModelCapabilities]; raw != "" {
			var caps []string
			if err := json.Unmarshal([]byte(raw), &caps); err == nil {
				d.ModelCapabilities = caps
			}
		}
		if d.DisplayName != "" || d.Description != "" || d.GenAIUseCase != "" || d.ContextWindow != "" || len(d.ModelCapabilities) > 0 {
			details = &d
		}
	}

	var urlPtr *apis.URL
	switch kind {
	case kindExternalModel:
		// ExternalModel models keep using status.endpoint as their URL.
		if endpoint != "" {
			if parsed, err := url.Parse(endpoint); err == nil {
				urlPtr = (*apis.URL)(parsed)
			}
		}
	default:
		// LLMInferenceService-backed models on BBR clusters share the gateway base URL.
		// Derive it from status.httpRouteHostnames[0] so all models point at the same
		// gateway entry-point instead of per-model path URLs.
		if hostnames, _, _ := unstructured.NestedStringSlice(u.Object, "status", "httpRouteHostnames"); len(hostnames) > 0 {
			if parsed, err := url.Parse("https://" + hostnames[0]); err == nil {
				urlPtr = (*apis.URL)(parsed)
			}
		} else if endpoint != "" {
			// Fall back to endpoint when httpRouteHostnames is not yet populated.
			if parsed, err := url.Parse(endpoint); err == nil {
				urlPtr = (*apis.URL)(parsed)
			}
		}
	}

	created := int64(0)
	if t := u.GetCreationTimestamp(); !t.IsZero() {
		created = t.Unix()
	}

	namespace := u.GetNamespace()
	// OwnedBy includes both namespace and MaaSModelRef name for dashboard display
	ownedBy := namespace + "/" + name
	return &Model{
		Model: openai.Model{
			ID:      modelID,
			Object:  "model",
			Created: created,
			OwnedBy: ownedBy,
		},
		Kind:    kind,
		URL:     urlPtr,
		Ready:   ready,
		Details: details,
	}
}
