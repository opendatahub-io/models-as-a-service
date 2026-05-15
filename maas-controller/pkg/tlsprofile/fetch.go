package tlsprofile

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var apiServerGVK = schema.GroupVersionKind{
	Group:   "config.openshift.io",
	Version: "v1",
	Kind:    "APIServer",
}

// FetchAPIServerTLSProfile reads the cluster APIServer resource and extracts
// the TLS security profile. Returns the default (Intermediate) profile when
// no profile is configured or the resource cannot be read.
func FetchAPIServerTLSProfile(ctx context.Context, c client.Reader) (ProfileSpec, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(apiServerGVK)

	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, obj); err != nil {
		return DefaultProfile(), fmt.Errorf("fetching APIServer/cluster: %w", err)
	}

	return parseProfileFromAPIServer(obj)
}

// parseProfileFromAPIServer extracts the TLS profile from an unstructured APIServer object.
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
		if spec, ok := LookupNamedProfile(pt); ok {
			return spec, nil
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
