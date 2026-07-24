package tlsprofile

import (
	"crypto/tls"

	confv1 "github.com/openshift/api/config/v1"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
)

// ProfileType defines an OpenShift TLS security profile type.
type ProfileType = confv1.TLSProfileType

const (
	ProfileOld          = confv1.TLSProfileOldType
	ProfileIntermediate = confv1.TLSProfileIntermediateType
	ProfileModern       = confv1.TLSProfileModernType
	ProfileCustom       = confv1.TLSProfileCustomType
)

// protocolVersion maps OpenShift TLS version values to Go crypto/tls constants.
var protocolVersion = map[confv1.TLSProtocolVersion]uint16{
	confv1.VersionTLS10: tls.VersionTLS10,
	confv1.VersionTLS11: tls.VersionTLS11,
	confv1.VersionTLS12: tls.VersionTLS12,
	confv1.VersionTLS13: tls.VersionTLS13,
}

// ProfileSpec holds the resolved cipher list and minimum TLS version for a profile.
type ProfileSpec struct {
	confv1.TLSProfileSpec

	Type ProfileType
}

// Settings holds the cluster TLS profile and the policy controlling whether
// non-legacy components must honor it.
type Settings struct {
	Profile   ProfileSpec
	Adherence confv1.TLSAdherencePolicy
}

// DefaultSettings returns the safe settings used until the OpenShift API can
// be read. NoOpinion has the same semantics as LegacyAdheringComponentsOnly.
func DefaultSettings() Settings {
	return Settings{
		Profile:   DefaultProfile(),
		Adherence: confv1.TLSAdherencePolicyNoOpinion,
	}
}

// AppliedProfile returns the cluster profile in StrictAllComponents mode and
// the default Intermediate profile in legacy mode. The library-go helper also
// treats unknown future adherence values as strict.
func (s Settings) AppliedProfile() ProfileSpec {
	if libgocrypto.ShouldHonorClusterTLSProfile(s.Adherence) {
		return s.Profile
	}
	return DefaultProfile()
}

// DefaultProfile returns a copy of the Intermediate profile, used when the
// cluster has no explicit TLS profile configured.
func DefaultProfile() ProfileSpec {
	return cloneNamedProfile(ProfileIntermediate)
}

// LookupNamedProfile returns a copy of the predefined OpenShift profile for a named type.
func LookupNamedProfile(t ProfileType) (ProfileSpec, bool) {
	p, ok := confv1.TLSProfiles[t]
	if !ok {
		return ProfileSpec{}, false
	}
	return cloneProfile(t, *p), true
}

func cloneNamedProfile(t ProfileType) ProfileSpec {
	return cloneProfile(t, *confv1.TLSProfiles[t])
}

func cloneProfile(t ProfileType, p confv1.TLSProfileSpec) ProfileSpec {
	p.Ciphers = append([]string(nil), p.Ciphers...)
	return ProfileSpec{
		Type:           t,
		TLSProfileSpec: p,
	}
}
