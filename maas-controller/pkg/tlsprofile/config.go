package tlsprofile

import (
	"crypto/tls"
)

// openSSLToGo maps OpenSSL/IANA cipher suite names (used by OpenShift profiles)
// to Go crypto/tls cipher suite IDs. TLS 1.3 suites are omitted because Go
// enables them unconditionally when TLS 1.3 is negotiated.
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

// tls13Ciphers are TLS 1.3 cipher suite names that Go manages automatically.
var tls13Ciphers = map[string]bool{
	"TLS_AES_128_GCM_SHA256":       true,
	"TLS_AES_256_GCM_SHA384":       true,
	"TLS_CHACHA20_POLY1305_SHA256": true,
}

// NewTLSConfigFunc returns a function that applies the profile's TLS settings
// to a tls.Config, and a list of cipher names that could not be mapped.
// The returned function is safe to use as a controller-runtime TLSOpts entry.
func NewTLSConfigFunc(spec ProfileSpec) (func(*tls.Config), []string) {
	minVersion, ok := protocolVersion[spec.MinTLSVersion]
	if !ok {
		minVersion = tls.VersionTLS12
	}

	var cipherSuites []uint16
	var unsupported []string

	for _, name := range spec.Ciphers {
		if tls13Ciphers[name] {
			continue
		}
		if id, ok := openSSLToGo[name]; ok {
			cipherSuites = append(cipherSuites, id)
		} else {
			unsupported = append(unsupported, name)
		}
	}

	// When all profile ciphers are TLS 1.3 (Go manages those unconditionally) or
	// unsupported, cipherSuites is empty. In that case we leave tls.Config.CipherSuites
	// nil so Go applies its secure defaults for TLS 1.2 negotiation. This is the safest
	// fallback — a restrictive but empty list would disable TLS 1.2 entirely.
	return func(c *tls.Config) {
		c.MinVersion = minVersion
		if len(cipherSuites) > 0 {
			c.CipherSuites = cipherSuites
		}
	}, unsupported
}
