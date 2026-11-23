package tlsprofile

import (
	"crypto/tls"
	"fmt"

	libgocrypto "github.com/openshift/library-go/pkg/crypto"
)

func cipherCode(name string) uint16 {
	// OpenShift custom profiles may contain either IANA or OpenSSL names.
	if code, err := libgocrypto.CipherSuite(name); err == nil {
		return code
	}

	ianaNames := libgocrypto.OpenSSLToIANACipherSuites([]string{name})
	if len(ianaNames) == 1 {
		if code, err := libgocrypto.CipherSuite(ianaNames[0]); err == nil {
			return code
		}
	}

	return 0
}

func isTLS13Cipher(code uint16) bool {
	switch code {
	case tls.TLS_AES_128_GCM_SHA256,
		tls.TLS_AES_256_GCM_SHA384,
		tls.TLS_CHACHA20_POLY1305_SHA256:
		return true
	default:
		return false
	}
}

// TLSConfigFromProfile converts a ProfileSpec into MinVersion and CipherSuites
// suitable for crypto/tls.Config. Returns cipher names that cannot be mapped or
// negotiated with the configured minimum version.
func TLSConfigFromProfile(spec ProfileSpec) (uint16, []uint16, []string, error) {
	minVer, ok := protocolVersion[spec.MinTLSVersion]
	if !ok {
		return 0, nil, nil, fmt.Errorf("unrecognized minimum TLS version %q", spec.MinTLSVersion)
	}

	ciphers := make([]uint16, 0, len(spec.Ciphers))
	var unsupported []string
	seen := make(map[uint16]struct{}, len(spec.Ciphers))

	for _, name := range spec.Ciphers {
		code := cipherCode(name)
		if code == 0 {
			unsupported = append(unsupported, name)
			continue
		}
		if minVer >= tls.VersionTLS13 {
			continue
		}
		if isTLS13Cipher(code) {
			unsupported = append(unsupported, name)
			continue
		}
		if !cipherSupportedAtMinVersion(code, minVer) {
			unsupported = append(unsupported, name)
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		ciphers = append(ciphers, code)
	}

	if len(spec.Ciphers) > 0 && minVer < tls.VersionTLS13 && len(ciphers) == 0 {
		return 0, nil, unsupported, fmt.Errorf(
			"no configured cipher suites are usable with minimum TLS version %q", spec.MinTLSVersion)
	}

	return minVer, ciphers, unsupported, nil
}

func cipherSupportedAtMinVersion(code, minVersion uint16) bool {
	for _, suite := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
		if suite.ID != code {
			continue
		}
		for _, version := range suite.SupportedVersions {
			if version >= minVersion {
				return true
			}
		}
		return false
	}
	return false
}
