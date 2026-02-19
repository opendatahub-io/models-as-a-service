package models

import (
	"net/url"

	"github.com/openai/openai-go/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/apis"
)

const (
	maasGroup    = "maas.opendatahub.io"
	maasVersion  = "v1alpha1"
	maasResource = "maasmodels"
)

// MaaSModelLister lists MaaSModel CRs from a cache (e.g. informer-backed). Used for GET /v1/models.
type MaaSModelLister interface {
	// List returns MaaSModel unstructured items in the given namespace.
	List(namespace string) ([]*unstructured.Unstructured, error)
}

// ListFromMaaSModelLister converts cached MaaSModel items to API models. Uses status.endpoint and status.phase.
func ListFromMaaSModelLister(lister MaaSModelLister, namespace string) ([]Model, error) {
	if lister == nil || namespace == "" {
		return nil, nil
	}
	items, err := lister.List(namespace)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(items))
	for _, u := range items {
		m := maasModelToModel(u)
		if m != nil {
			out = append(out, *m)
		}
	}
	return out, nil
}

// GVR returns the GroupVersionResource for MaaSModel CRs.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: maasGroup, Version: maasVersion, Resource: maasResource}
}

// maasModelToModel converts a MaaSModel unstructured to a Model for the API.
func maasModelToModel(u *unstructured.Unstructured) *Model {
	if u == nil {
		return nil
	}
	name := u.GetName()
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	endpoint, _, _ := unstructured.NestedString(u.Object, "status", "endpoint")
	ready := phase == "Ready"
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "modelRef", "kind")
	if kind == "" {
		kind = "llmisvc"
	}

	var urlPtr *apis.URL
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err == nil {
			urlPtr = (*apis.URL)(parsed)
		}
	}

	created := int64(0)
	if t := u.GetCreationTimestamp(); !t.IsZero() {
		created = t.Unix()
	}

	return &Model{
		Model: openai.Model{
			ID:      name,
			Object:  "model",
			Created: created,
			OwnedBy: u.GetNamespace(),
		},
		Kind:  kind,
		URL:   urlPtr,
		Ready: ready,
	}
}
