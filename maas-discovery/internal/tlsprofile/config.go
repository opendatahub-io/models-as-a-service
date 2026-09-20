package tlsprofile

import (
	"crypto/tls"

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
// suitable for crypto/tls.Config. Returns unsupported cipher names that could
// not be mapped.
func TLSConfigFromProfile(spec ProfileSpec) (uint16, []uint16, []string) {
	minVer, ok := protocolVersion[spec.MinTLSVersion]
	if !ok {
		minVer = tls.VersionTLS12
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
		if isTLS13Cipher(code) {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		ciphers = append(ciphers, code)
	}

	// When all profile ciphers are TLS 1.3 (Go manages those unconditionally) or
	// unsupported, ciphers is empty. The caller should leave tls.Config.CipherSuites
	// nil so Go applies its secure defaults for TLS 1.2 negotiation.
	return minVer, ciphers, unsupported
}
