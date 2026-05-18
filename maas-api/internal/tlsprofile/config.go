package tlsprofile

import (
	"crypto/tls"
)

// openSSLToGo maps OpenSSL/IANA cipher suite names to Go crypto/tls IDs.
// TLS 1.3 suites are omitted because Go enables them unconditionally.
var openSSLToGo = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-AES128-SHA256":     tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-RSA-AES128-SHA256":       tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-ECDSA-AES128-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"ECDHE-RSA-AES128-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"ECDHE-ECDSA-AES256-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	"ECDHE-RSA-AES256-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	"AES128-GCM-SHA256":             tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"AES256-GCM-SHA384":             tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"AES128-SHA256":                 tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
	"AES128-SHA":                    tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	"AES256-SHA":                    tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	"DES-CBC3-SHA":                  tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

var tls13Ciphers = map[string]bool{
	"TLS_AES_128_GCM_SHA256":       true,
	"TLS_AES_256_GCM_SHA384":       true,
	"TLS_CHACHA20_POLY1305_SHA256": true,
}

// TLSConfigFromProfile converts a ProfileSpec into MinVersion and CipherSuites
// suitable for crypto/tls.Config. Returns unsupported cipher names that could
// not be mapped.
func TLSConfigFromProfile(spec ProfileSpec) (uint16, []uint16, []string) {
	minVer, ok := protocolVersion[spec.MinTLSVersion]
	if !ok {
		minVer = tls.VersionTLS12
	}

	var ciphers []uint16
	var unsupported []string

	for _, name := range spec.Ciphers {
		if tls13Ciphers[name] {
			continue
		}
		if id, ok := openSSLToGo[name]; ok {
			ciphers = append(ciphers, id)
		} else {
			unsupported = append(unsupported, name)
		}
	}

	// When all profile ciphers are TLS 1.3 (Go manages those unconditionally) or
	// unsupported, ciphers is empty. The caller should leave tls.Config.CipherSuites
	// nil so Go applies its secure defaults for TLS 1.2 negotiation.
	return minVer, ciphers, unsupported
}
