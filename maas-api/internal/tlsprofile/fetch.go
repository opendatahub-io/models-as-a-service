package tlsprofile

import (
	"context"
	"errors"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configclientset "github.com/openshift/client-go/config/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// FetchTLSProfile reads the cluster APIServer resource and extracts the TLS
// security profile. Returns the default (Intermediate) profile when no profile
// is configured or the resource cannot be read.
func FetchTLSProfile(ctx context.Context, restConfig *rest.Config) (ProfileSpec, error) {
	if restConfig == nil {
		return DefaultProfile(), errors.New("restConfig must not be nil")
	}

	configClient, err := configclientset.NewForConfig(restConfig)
	if err != nil {
		return DefaultProfile(), fmt.Errorf("creating OpenShift config client: %w", err)
	}

	apiServer, err := configClient.ConfigV1().APIServers().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return DefaultProfile(), fmt.Errorf("fetching APIServer/cluster: %w", err)
	}

	return profileFromAPIServer(apiServer)
}

func profileFromAPIServer(apiServer *configv1.APIServer) (ProfileSpec, error) {
	if apiServer == nil {
		return DefaultProfile(), errors.New("APIServer must not be nil")
	}
	return profileFromTLSSecurityProfile(apiServer.Spec.TLSSecurityProfile)
}

func profileFromTLSSecurityProfile(profile *configv1.TLSSecurityProfile) (ProfileSpec, error) {
	if profile == nil || profile.Type == "" {
		return DefaultProfile(), nil
	}

	if profile.Type != ProfileCustom {
		if spec, ok := LookupNamedProfile(profile.Type); ok {
			return spec, nil
		}
		return DefaultProfile(), fmt.Errorf("unrecognized TLS profile type %q", profile.Type)
	}

	if profile.Custom == nil {
		return DefaultProfile(), errors.New("custom profile type set but spec.tlsSecurityProfile.custom is missing")
	}

	spec := profile.Custom.TLSProfileSpec
	if len(spec.Ciphers) == 0 {
		return DefaultProfile(), errors.New("custom profile has empty cipher list")
	}
	if spec.MinTLSVersion == "" {
		spec.MinTLSVersion = configv1.VersionTLS12
	}
	if _, ok := protocolVersion[spec.MinTLSVersion]; !ok {
		return DefaultProfile(), fmt.Errorf("custom profile has unrecognized minTLSVersion %q", spec.MinTLSVersion)
	}

	return cloneProfile(ProfileCustom, spec), nil
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
