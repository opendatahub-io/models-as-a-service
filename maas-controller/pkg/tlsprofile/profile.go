package tlsprofile

import "crypto/tls"

// ProfileType defines an OpenShift TLS security profile type.
type ProfileType string

const (
	ProfileOld          ProfileType = "Old"
	ProfileIntermediate ProfileType = "Intermediate"
	ProfileModern       ProfileType = "Modern"
	ProfileCustom       ProfileType = "Custom"
)

// ProtocolVersion maps OpenShift TLS version strings to Go crypto/tls constants.
var ProtocolVersion = map[string]uint16{
	"VersionTLS10": tls.VersionTLS10,
	"VersionTLS11": tls.VersionTLS11,
	"VersionTLS12": tls.VersionTLS12,
	"VersionTLS13": tls.VersionTLS13,
}

// ProfileSpec holds the resolved cipher list and minimum TLS version for a profile.
type ProfileSpec struct {
	Type          ProfileType
	Ciphers       []string
	MinTLSVersion string
}

// Predefined profiles based on Mozilla Server Side TLS guidelines v5.7,
// matching openshift/api/config/v1.TLSProfiles. These are hardcoded to avoid
// importing openshift/api which requires k8s.io/api v0.35+ (our tree is v0.33).
// If OpenShift changes its profile definitions, update these to match.
// See: https://ssl-config.mozilla.org/guidelines/5.7.json
var profiles = map[ProfileType]ProfileSpec{
	ProfileOld: {
		Type: ProfileOld,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
			"ECDHE-ECDSA-AES128-GCM-SHA256",
			"ECDHE-RSA-AES128-GCM-SHA256",
			"ECDHE-ECDSA-AES256-GCM-SHA384",
			"ECDHE-RSA-AES256-GCM-SHA384",
			"ECDHE-ECDSA-CHACHA20-POLY1305",
			"ECDHE-RSA-CHACHA20-POLY1305",
			"ECDHE-ECDSA-AES128-SHA256",
			"ECDHE-RSA-AES128-SHA256",
			"ECDHE-ECDSA-AES128-SHA",
			"ECDHE-RSA-AES128-SHA",
			"ECDHE-ECDSA-AES256-SHA",
			"ECDHE-RSA-AES256-SHA",
			"AES128-GCM-SHA256",
			"AES256-GCM-SHA384",
			"AES128-SHA256",
			"AES128-SHA",
			"AES256-SHA",
			"DES-CBC3-SHA",
		},
		MinTLSVersion: "VersionTLS10",
	},
	ProfileIntermediate: {
		Type: ProfileIntermediate,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
			"ECDHE-ECDSA-AES128-GCM-SHA256",
			"ECDHE-RSA-AES128-GCM-SHA256",
			"ECDHE-ECDSA-AES256-GCM-SHA384",
			"ECDHE-RSA-AES256-GCM-SHA384",
			"ECDHE-ECDSA-CHACHA20-POLY1305",
			"ECDHE-RSA-CHACHA20-POLY1305",
		},
		MinTLSVersion: "VersionTLS12",
	},
	ProfileModern: {
		Type: ProfileModern,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
		},
		MinTLSVersion: "VersionTLS13",
	},
}

// DefaultProfile is used when the cluster has no explicit TLS profile configured.
var DefaultProfile = profiles[ProfileIntermediate]

// LookupNamedProfile returns the predefined profile for a named type.
func LookupNamedProfile(t ProfileType) (ProfileSpec, bool) {
	p, ok := profiles[t]
	return p, ok
}
