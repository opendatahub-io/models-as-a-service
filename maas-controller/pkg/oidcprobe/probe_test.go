package oidcprobe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const jwksBody = `{"keys":[{"kty":"RSA","n":"sO2wSSNodesDoNotMatterForTheProbe","e":"AQAB"}]}`

// newOIDCServer starts an HTTPS test server whose discovery document reports
// the given issuer. The server URL is not known before startup, so the
// handler resolves the issuer lazily through issuer(). jwksStatus and
// jwksBody control the JWKS response.
func newOIDCServer(t *testing.T, issuer func() string, jwksStatus int, jwksBody string) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/.well-known/openid-configuration"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, issuer(), issuer()+"/certs")
		case strings.HasSuffix(req.URL.Path, "/certs"):
			if jwksStatus != 0 && jwksStatus != http.StatusOK {
				w.WriteHeader(jwksStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jwksBody)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)

	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server did not expose a certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, pool
}

func TestProbe_Available(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer }, 0, jwksBody)
	issuer = srv.URL + "/realms/maas"

	res := Probe(context.Background(), issuer, pool)
	if !res.OK || res.Reason != ReasonAvailable {
		t.Fatalf("expected OK/Available, got %+v", res)
	}
}

func TestProbe_TrailingSlashIssuerStillMatches(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer + "/" }, 0, jwksBody)
	issuer = srv.URL + "/realms/maas"

	res := Probe(context.Background(), issuer, pool)
	if !res.OK || res.Reason != ReasonAvailable {
		t.Fatalf("expected OK/Available for trailing-slash difference, got %+v", res)
	}
}

func TestProbe_IssuerUnreachable(t *testing.T) {
	// Grab a free port and close the listener so connections are refused.
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected *net.TCPAddr, got %T", l.Addr())
	}
	port := addr.Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	res := Probe(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", port), nil)
	if res.OK || res.Reason != ReasonIssuerUnreachable {
		t.Fatalf("expected IssuerUnreachable, got %+v", res)
	}
}

func TestProbe_TLSUntrustedWithoutPoolAndAvailableWithPool(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer }, 0, jwksBody)
	issuer = srv.URL

	emptyPool := x509.NewCertPool()
	res := Probe(context.Background(), issuer, emptyPool)
	if res.OK || res.Reason != ReasonTLSUntrusted {
		t.Fatalf("expected TLSUntrusted with empty pool, got %+v", res)
	}

	res = Probe(context.Background(), issuer, pool)
	if !res.OK || res.Reason != ReasonAvailable {
		t.Fatalf("expected Available with server CA in pool, got %+v", res)
	}
}

func TestProbe_DiscoveryFailedOn404(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	res := Probe(context.Background(), srv.URL, nil)
	if res.OK || res.Reason != ReasonDiscoveryFailed {
		t.Fatalf("expected DiscoveryFailed on 404, got %+v", res)
	}
}

func TestProbe_DiscoveryFailedOnInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()

	res := Probe(context.Background(), srv.URL, nil)
	if res.OK || res.Reason != ReasonDiscoveryFailed {
		t.Fatalf("expected DiscoveryFailed on invalid JSON, got %+v", res)
	}
}

func TestProbe_IssuerMismatch(t *testing.T) {
	srv, pool := newOIDCServer(t, func() string { return "https://other.example.com/realms/x" }, 0, jwksBody)

	res := Probe(context.Background(), srv.URL+"/realms/x", pool)
	if res.OK || res.Reason != ReasonIssuerMismatch {
		t.Fatalf("expected IssuerMismatch, got %+v", res)
	}
}

func TestProbe_JWKSUnavailableOn503(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer }, http.StatusServiceUnavailable, jwksBody)
	issuer = srv.URL

	res := Probe(context.Background(), issuer, pool)
	if res.OK || res.Reason != ReasonJWKSUnavailable {
		t.Fatalf("expected JWKSUnavailable on 503, got %+v", res)
	}
}

func TestProbe_JWKSUnavailableOnEmptyKeys(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer }, 0, `{"keys":[]}`)
	issuer = srv.URL

	res := Probe(context.Background(), issuer, pool)
	if res.OK || res.Reason != ReasonJWKSUnavailable {
		t.Fatalf("expected JWKSUnavailable on empty keys, got %+v", res)
	}
}

func TestProbe_JWKSUnavailableOnNullKeys(t *testing.T) {
	var issuer string
	srv, pool := newOIDCServer(t, func() string { return issuer }, 0, `{"keys":[null]}`)
	issuer = srv.URL

	res := Probe(context.Background(), issuer, pool)
	if res.OK || res.Reason != ReasonJWKSUnavailable {
		t.Fatalf("expected JWKSUnavailable on null keys, got %+v", res)
	}
}

func TestProbe_DiscoveryFailedOnNon200Status(t *testing.T) {
	// A 201 with a valid discovery body must still count as a failed
	// discovery: only HTTP 200 is accepted.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"issuer":"x","jwks_uri":"x"}`)
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	res := Probe(context.Background(), srv.URL, pool)
	if res.OK || res.Reason != ReasonDiscoveryFailed {
		t.Fatalf("expected DiscoveryFailed on 201, got %+v", res)
	}
}

func TestProbe_JWKSUnavailableOnInsecureJWKSURI(t *testing.T) {
	// An HTTPS issuer whose discovery advertises an http:// jwks_uri must be
	// rejected rather than probed in cleartext.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/.well-known/openid-configuration") {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"issuer":   "https://" + req.Host + "/",
				"jwks_uri": "http://" + req.Host + "/certs",
			}); err != nil {
				t.Errorf("encode discovery document: %v", err)
			}
			return
		}
		http.NotFound(w, req)
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	res := Probe(context.Background(), srv.URL, pool)
	if res.OK || res.Reason != ReasonJWKSUnavailable {
		t.Fatalf("expected JWKSUnavailable on insecure jwks_uri, got %+v", res)
	}
}

func TestProbe_IssuerUnreachableOnTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	// Defers run LIFO: close(release) runs first so srv.Close() does not wait
	// on the blocked handler.
	defer srv.Close()
	defer close(release)

	start := time.Now()
	res := ProbeWithTimeout(context.Background(), srv.URL, nil, 200*time.Millisecond)
	elapsed := time.Since(start)
	if res.OK || res.Reason != ReasonIssuerUnreachable {
		t.Fatalf("expected IssuerUnreachable on timeout, got %+v", res)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe did not honor the timeout: took %s", elapsed)
	}
}
