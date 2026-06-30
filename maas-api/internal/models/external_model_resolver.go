package models

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ExternalModelResolver resolves ExternalModel CRD names to spec.modelName.
type ExternalModelResolver struct {
	client dynamic.Interface
}

// NewExternalModelResolver creates a new resolver backed by a dynamic client.
func NewExternalModelResolver(client dynamic.Interface) *ExternalModelResolver {
	return &ExternalModelResolver{client: client}
}

// ResolveModelName looks up an inference.opendatahub.io ExternalModel and returns spec.modelName.
func (r *ExternalModelResolver) ResolveModelName(namespace, name string) string {
	if r.client == nil {
		return ""
	}
	gvr := schema.GroupVersionResource{
		Group:    "inference.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "externalmodels",
	}
	obj, err := r.client.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	modelName, _, _ := unstructured.NestedString(obj.Object, "spec", "modelName")
	return modelName
}
