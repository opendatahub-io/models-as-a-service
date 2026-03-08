package subscription

import "k8s.io/apimachinery/pkg/runtime/schema"

const (
	maasGroup    = "maas.opendatahub.io"
	maasVersion  = "v1alpha1"
	maasResource = "maassubscriptions"
)

// GVR returns the GroupVersionResource for MaaSSubscription CRs.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: maasGroup, Version: maasVersion, Resource: maasResource}
}
