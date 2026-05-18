package tlsprofile

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		return DefaultProfile(), fmt.Errorf("creating dynamic client: %w", err)
	}

	obj, err := dynClient.Resource(apiServerGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return DefaultProfile(), fmt.Errorf("fetching APIServer/cluster: %w", err)
	}

	return parseProfileFromAPIServer(obj)
}

func parseProfileFromAPIServer(obj *unstructured.Unstructured) (ProfileSpec, error) {
	profileObj, found, err := unstructured.NestedMap(obj.Object, "spec", "tlsSecurityProfile")
	if err != nil {
		return DefaultProfile(), fmt.Errorf("reading spec.tlsSecurityProfile: %w", err)
	}
	if !found || profileObj == nil {
		return DefaultProfile(), nil
	}

	profileType, _, typeErr := unstructured.NestedString(profileObj, "type")
	if typeErr != nil {
		return DefaultProfile(), fmt.Errorf("parsing spec.tlsSecurityProfile.type: %w", typeErr)
	}
	if profileType == "" {
		return DefaultProfile(), nil
	}

	pt := ProfileType(profileType)

	if pt != ProfileCustom {
		if spec, ok := profiles[pt]; ok {
			return cloneProfile(spec), nil
		}
		return DefaultProfile(), fmt.Errorf("unrecognized TLS profile type %q", profileType)
	}

	return parseCustomProfile(profileObj)
}

func parseCustomProfile(profileObj map[string]any) (ProfileSpec, error) {
	customObj, found, err := unstructured.NestedMap(profileObj, "custom")
	if err != nil || !found {
		return DefaultProfile(), errors.New("custom profile type set but spec.tlsSecurityProfile.custom is missing")
	}

	ciphersRaw, ciphersFound, ciphersErr := unstructured.NestedStringSlice(customObj, "ciphers")
	if ciphersErr != nil {
		return DefaultProfile(), fmt.Errorf("parsing custom profile ciphers: %w", ciphersErr)
	}
	minVersion, _, minVerErr := unstructured.NestedString(customObj, "minTLSVersion")
	if minVerErr != nil {
		return DefaultProfile(), fmt.Errorf("parsing custom profile minTLSVersion: %w", minVerErr)
	}

	if !ciphersFound || len(ciphersRaw) == 0 {
		return DefaultProfile(), errors.New("custom profile has empty cipher list")
	}
	if minVersion == "" {
		minVersion = "VersionTLS12"
	}
	if _, ok := protocolVersion[minVersion]; !ok {
		return DefaultProfile(), fmt.Errorf("custom profile has unrecognized minTLSVersion %q", minVersion)
	}

	return ProfileSpec{
		Type:          ProfileCustom,
		Ciphers:       ciphersRaw,
		MinTLSVersion: minVersion,
	}, nil
}

// IsAPIUnavailable returns true when the error indicates the config.openshift.io
// API group or APIServer resource is not present (non-OpenShift cluster), as
// opposed to a transient failure that should be retried.
func IsAPIUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err)
}
