package cert_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cert"
)

func TestGenerate(t *testing.T) {
	tlsCert, err := cert.Generate("test-discovery")
	require.NoError(t, err)
	require.NotEmpty(t, tlsCert.Certificate)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	assert.Equal(t, []string{"test-discovery"}, leaf.Subject.Organization)
	assert.Equal(t, x509.SHA256WithRSA, leaf.SignatureAlgorithm)
	assert.True(t, leaf.NotAfter.After(time.Now().Add(cert.Duration)))
	assert.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
}

func TestGenerateSANs(t *testing.T) {
	tlsCert, err := cert.Generate("test-discovery")
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	assert.Contains(t, leaf.DNSNames, "localhost")
	assert.Contains(t, leaf.DNSNames, "test-discovery")
	assert.Contains(t, leaf.IPAddresses, net.IPv4(127, 0, 0, 1).To4())
	assert.Contains(t, leaf.IPAddresses, net.IPv6loopback)

	assert.NoError(t, leaf.VerifyHostname("localhost"))
	assert.NoError(t, leaf.VerifyHostname("test-discovery"))
}

func TestGenerateTLSHandshake(t *testing.T) {
	tlsCert, err := cert.Generate("test-discovery")
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
	clientCfg := &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			done <- aerr
			return
		}
		defer conn.Close()
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			done <- aerr
			return
		}
		done <- tlsConn.Handshake()
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, <-done)
}
