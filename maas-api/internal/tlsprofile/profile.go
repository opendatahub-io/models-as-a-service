package tlsprofile

import (
	"crypto/tls"

	configv1 "github.com/openshift/api/config/v1"
)

// ProfileType defines an OpenShift TLS security profile type.
type ProfileType = configv1.TLSProfileType

const (
	ProfileOld          = configv1.TLSProfileOldType
	ProfileIntermediate = configv1.TLSProfileIntermediateType
	ProfileModern       = configv1.TLSProfileModernType
	ProfileCustom       = configv1.TLSProfileCustomType
)

// protocolVersion maps OpenShift TLS version values to Go crypto/tls constants.
var protocolVersion = map[configv1.TLSProtocolVersion]uint16{
	configv1.VersionTLS10: tls.VersionTLS10,
	configv1.VersionTLS11: tls.VersionTLS11,
	configv1.VersionTLS12: tls.VersionTLS12,
	configv1.VersionTLS13: tls.VersionTLS13,
}

// ProfileSpec holds the resolved cipher list and minimum TLS version for a profile.
type ProfileSpec struct {
	Type ProfileType
	configv1.TLSProfileSpec
}

// DefaultProfile returns a copy of the Intermediate profile, used when the
// cluster has no explicit TLS profile configured.
func DefaultProfile() ProfileSpec {
	return cloneNamedProfile(ProfileIntermediate)
}

// LookupNamedProfile returns a copy of the predefined OpenShift profile for a named type.
func LookupNamedProfile(t ProfileType) (ProfileSpec, bool) {
	p, ok := configv1.TLSProfiles[t]
	if !ok {
		return ProfileSpec{}, false
	}
	return cloneProfile(t, *p), true
}

func cloneNamedProfile(t ProfileType) ProfileSpec {
	return cloneProfile(t, *configv1.TLSProfiles[t])
}

func cloneProfile(t ProfileType, p configv1.TLSProfileSpec) ProfileSpec {
	p.Ciphers = append([]string(nil), p.Ciphers...)
	return ProfileSpec{
		Type:           t,
		TLSProfileSpec: p,
	}
}
