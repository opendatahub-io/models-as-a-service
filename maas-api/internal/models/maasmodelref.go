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
	maasResource = "maasmodelrefs"
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

	namespace := u.GetNamespace()
	// OwnedBy includes both namespace and MaaSModelRef name for dashboard display
	ownedBy := namespace + "/" + name
	return &Model{
		Model: openai.Model{
			ID:      name,
			Object:  "model",
			Created: created,
			OwnedBy: ownedBy,
		},
		Kind:  kind,
		URL:   urlPtr,
		Ready: ready,
	}
}
