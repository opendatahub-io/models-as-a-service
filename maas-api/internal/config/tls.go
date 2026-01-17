package config

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"

	"k8s.io/utils/env"
)

const (
	tlsVersion12 = "1.2"
	tlsVersion13 = "1.3"
)

type TLSVersion uint16

var _ flag.Value = (*TLSVersion)(nil)

func (v *TLSVersion) String() string {
	switch uint16(*v) {
	case tls.VersionTLS12:
		return tlsVersion12
	case tls.VersionTLS13:
		return tlsVersion13
	default:
		return tlsVersion12
	}
}

func (v *TLSVersion) Set(s string) error {
	switch s {
	case tlsVersion12:
		*v = TLSVersion(tls.VersionTLS12)
	case tlsVersion13:
		*v = TLSVersion(tls.VersionTLS13)
	default:
		return fmt.Errorf("unsupported TLS version %q: must be %s or %s", s, tlsVersion12, tlsVersion13)
	}
	return nil
}

func (v *TLSVersion) Value() uint16 {
	return uint16(*v)
}

// TLSConfig holds TLS-related configuration.
type TLSConfig struct {
	Cert       string     // Path to TLS certificate
	Key        string     // Path to TLS private key
	SelfSigned bool       // Generate self-signed certificate
	MinVersion TLSVersion // Minimum TLS version
}

// Enabled returns true if TLS is configured (either with certs or self-signed).
func (t *TLSConfig) Enabled() bool {
	return t.HasCerts() || t.SelfSigned
}

// HasCerts returns true if certificate files are configured.
func (t *TLSConfig) HasCerts() bool {
	return t.Cert != "" && t.Key != ""
}

// loadTLSConfig loads TLS configuration from environment variables.
func loadTLSConfig() TLSConfig {
	selfSigned, _ := env.GetBool("TLS_SELF_SIGNED", false)
	return TLSConfig{
		Cert:       env.GetString("TLS_CERT", ""),
		Key:        env.GetString("TLS_KEY", ""),
		SelfSigned: selfSigned,
		MinVersion: TLSVersion(tls.VersionTLS12),
	}
}

// bindFlags binds TLS flags to the flagset.
func (t *TLSConfig) bindFlags(fs *flag.FlagSet) {
	fs.StringVar(&t.Cert, "tls-cert", t.Cert, "Path to TLS certificate")
	fs.StringVar(&t.Key, "tls-key", t.Key, "Path to TLS private key")
	fs.BoolVar(&t.SelfSigned, "tls-self-signed", t.SelfSigned, "Generate self-signed certificate")
	fs.Var(&t.MinVersion, "tls-min-version", "Minimum TLS version: 1.2 or 1.3 (default: 1.2)")
}

// validate validates TLS configuration.
func (t *TLSConfig) validate() error {
	// Validate that cert and key are provided together
	if (t.Cert != "" && t.Key == "") || (t.Cert == "" && t.Key != "") {
		return errors.New("--tls-cert and --tls-key must both be provided together")
	}

	if t.HasCerts() {
		t.SelfSigned = false
	}

	if envVal := env.GetString("TLS_MIN_VERSION", ""); envVal != "" {
		if err := t.MinVersion.Set(envVal); err != nil {
			return err
		}
	}

	return nil
}
