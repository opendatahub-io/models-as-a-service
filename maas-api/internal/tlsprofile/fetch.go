package tlsprofile

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var apiServerGVR = schema.GroupVersionResource{
	Group:    "config.openshift.io",
	Version:  "v1",
	Resource: "apiservers",
}

// FetchTLSProfile reads the cluster APIServer resource and extracts the TLS
// security profile. Returns the default (Intermediate) profile when no profile
// is configured or the resource cannot be read.
func FetchTLSProfile(ctx context.Context, restConfig *rest.Config) (ProfileSpec, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return DefaultProfile, fmt.Errorf("creating dynamic client: %w", err)
	}

	obj, err := dynClient.Resource(apiServerGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return DefaultProfile, fmt.Errorf("fetching APIServer/cluster: %w", err)
	}

	return parseProfileFromAPIServer(obj)
}

func parseProfileFromAPIServer(obj *unstructured.Unstructured) (ProfileSpec, error) {
	profileObj, found, err := unstructured.NestedMap(obj.Object, "spec", "tlsSecurityProfile")
	if err != nil {
		return DefaultProfile, fmt.Errorf("reading spec.tlsSecurityProfile: %w", err)
	}
	if !found || profileObj == nil {
		return DefaultProfile, nil
	}

	profileType, _, _ := unstructured.NestedString(profileObj, "type")
	if profileType == "" {
		return DefaultProfile, nil
	}

	pt := ProfileType(profileType)

	if pt != ProfileCustom {
		if spec, ok := profiles[pt]; ok {
			return spec, nil
		}
		return DefaultProfile, nil
	}

	return parseCustomProfile(profileObj)
}

func parseCustomProfile(profileObj map[string]any) (ProfileSpec, error) {
	customObj, found, err := unstructured.NestedMap(profileObj, "custom")
	if err != nil || !found {
		return DefaultProfile, errors.New("custom profile type set but spec.tlsSecurityProfile.custom is missing")
	}

	ciphersRaw, _, _ := unstructured.NestedStringSlice(customObj, "ciphers")
	minVersion, _, _ := unstructured.NestedString(customObj, "minTLSVersion")

	if len(ciphersRaw) == 0 {
		return DefaultProfile, errors.New("custom profile has empty cipher list")
	}
	if minVersion == "" {
		minVersion = "VersionTLS12"
	}

	return ProfileSpec{
		Type:          ProfileCustom,
		Ciphers:       ciphersRaw,
		MinTLSVersion: minVersion,
	}, nil
}
