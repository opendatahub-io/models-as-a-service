/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package oidcprobe probes an OIDC issuer's configuration endpoints
// (discovery document and JWKS) and classifies the outcome into a stable,
// machine-readable reason suitable for a status condition.
//
// This package intentionally imports no Kubernetes or MaaS types so it can be
// copied as-is when AITenant moves to ai-gateway-controller.
package oidcprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Reason values are the API contract of the AITenant OIDCReady condition.
// Keep them stable: admins and automation key off these identifiers, and the
// planned move of the condition to ai-gateway-controller must not change them.
const (
	// ReasonAvailable indicates discovery and JWKS are reachable and the
	// discovery issuer matches spec.oidc.issuerUrl.
	ReasonAvailable = "Available"
	// ReasonClientConfigIncomplete indicates the AITenant OIDC client
	// configuration is incomplete (empty clientId or an unusable
	// clientSecretRef). Reported by the controller, not by Probe.
	ReasonClientConfigIncomplete = "ClientConfigIncomplete"
	// ReasonIssuerUnreachable indicates a network-level failure reaching the
	// issuer (DNS failure, connection refused, timeout).
	ReasonIssuerUnreachable = "IssuerUnreachable"
	// ReasonTLSUntrusted indicates the issuer's TLS certificate is not trusted
	// by the probe's CA pool (unknown authority, hostname mismatch, expired).
	ReasonTLSUntrusted = "TLSUntrusted"
	// ReasonDiscoveryFailed indicates the discovery endpoint returned a
	// non-200 response or invalid JSON.
	ReasonDiscoveryFailed = "DiscoveryFailed"
	// ReasonIssuerMismatch indicates the discovery document's issuer does not
	// match spec.oidc.issuerUrl.
	ReasonIssuerMismatch = "IssuerMismatch"
	// ReasonJWKSUnavailable indicates the JWKS endpoint is missing, returned a
	// non-200 response, or contains no keys.
	ReasonJWKSUnavailable = "JWKSUnavailable"
)

const (
	// requestTimeout is the per-request timeout used by Probe.
	requestTimeout = 5 * time.Second
	// maxBodyBytes caps how much of a response body is read.
	maxBodyBytes = 1 << 20 // 1 MiB
	// maxMessageLen caps the underlying error text embedded in messages.
	maxMessageLen = 200
)

// Result is the outcome of an OIDC probe. Reason is one of the Reason*
// constants; Message is human-readable, never contains secrets, and has URLs
// stripped of query strings.
type Result struct {
	OK      bool
	Reason  string
	Message string
}

// Probe fetches the issuer's discovery document, verifies the reported issuer
// matches issuerURL (ignoring a trailing slash), fetches the JWKS and requires
// at least one key. Each request gets a 5s timeout; redirects to a different
// host are refused. A nil roots pool uses the system roots.
func Probe(ctx context.Context, issuerURL string, roots *x509.CertPool) Result {
	return ProbeWithTimeout(ctx, issuerURL, roots, requestTimeout)
}

// ProbeWithTimeout is Probe with a per-request timeout override (used by
// tests; production code should call Probe).
func ProbeWithTimeout(ctx context.Context, issuerURL string, roots *x509.CertPool, timeout time.Duration) Result {
	base, err := url.Parse(issuerURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return Result{
			OK:      false,
			Reason:  ReasonDiscoveryFailed,
			Message: fmt.Sprintf("OIDC issuer URL %q is not a valid absolute URL", sanitizeURL(issuerURL)),
		}
	}

	// Clone the default transport rather than building one from scratch: this
	// keeps the platform defaults (connection pooling, HTTP/2, proxy support
	// via HTTPS_PROXY/HTTP_PROXY) and only adds the CA pool and TLS floor for
	// the probe.
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Unreachable in practice: http.DefaultTransport is documented to be
		// a *http.Transport. Fall back to a proxy-aware transport anyway.
		defaultTransport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport := defaultTransport.Clone()
	// The transport (and its connection pool) is per-probe, so release its
	// idle connections when the probe returns instead of waiting for the
	// idle-connection timeout to reap them.
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing redirect to different host %s", sanitizeURL(req.URL.String()))
			}
			// Never follow a redirect that would downgrade an HTTPS request
			// to cleartext HTTP.
			if via[0].URL.Scheme == "https" && req.URL.Scheme == "http" {
				return errors.New("refusing redirect from HTTPS to HTTP")
			}
			return nil
		},
	}

	discoveryURL := strings.TrimSuffix(base.String(), "/") + "/.well-known/openid-configuration"
	body, res := fetch(ctx, client, discoveryURL)
	if !res.OK {
		if res.Reason == ReasonIssuerUnreachable || res.Reason == ReasonTLSUntrusted {
			return res
		}
		return Result{
			OK:      false,
			Reason:  ReasonDiscoveryFailed,
			Message: fmt.Sprintf("OIDC discovery returned %s from %s", res.Message, sanitizeURL(discoveryURL)),
		}
	}

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Result{
			OK:      false,
			Reason:  ReasonDiscoveryFailed,
			Message: fmt.Sprintf("OIDC discovery from %s returned invalid JSON: %s", sanitizeURL(discoveryURL), truncate(err.Error())),
		}
	}

	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(issuerURL, "/") {
		return Result{
			OK:      false,
			Reason:  ReasonIssuerMismatch,
			Message: fmt.Sprintf("Discovery issuer %s does not match spec.oidc.issuerUrl %s", sanitizeURL(doc.Issuer), sanitizeURL(issuerURL)),
		}
	}

	if doc.JWKSURI == "" {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("OIDC discovery from %s has no jwks_uri", sanitizeURL(discoveryURL)),
		}
	}
	jwksURL, err := resolveReference(base, doc.JWKSURI)
	if err != nil {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("OIDC discovery from %s has an invalid jwks_uri %q", sanitizeURL(discoveryURL), sanitizeURL(doc.JWKSURI)),
		}
	}
	// An HTTPS issuer must not be downgraded to a cleartext JWKS endpoint.
	if base.Scheme == "https" && strings.HasPrefix(jwksURL, "http://") {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("OIDC discovery from %s has an insecure http jwks_uri %s", sanitizeURL(discoveryURL), sanitizeURL(doc.JWKSURI)),
		}
	}
	// SSRF hygiene: the discovery document (not the admin) chooses the
	// jwks_uri, so when the issuer itself is not an internal address, refuse
	// obvious internal jwks addresses — a public issuer must not be able to
	// point the probe at the cluster's internal network. When the issuer is
	// itself internal (a deliberate admin choice in dev/test setups), an
	// internal jwks_uri is consistent with that choice and is allowed. See
	// resolveReference for the trust-boundary rationale.
	if jwksRef, err := url.Parse(jwksURL); err == nil && !rejectPrivateAddress(base) && rejectPrivateAddress(jwksRef) {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("OIDC discovery from %s has a jwks_uri pointing at an internal address %s", sanitizeURL(discoveryURL), sanitizeURL(doc.JWKSURI)),
		}
	}

	body, res = fetch(ctx, client, jwksURL)
	if !res.OK {
		if res.Reason == ReasonTLSUntrusted {
			return res
		}
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("Cannot fetch OIDC signing keys from %s: %s", sanitizeURL(jwksURL), res.Message),
		}
	}

	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("Cannot parse OIDC signing keys from %s: %s", sanitizeURL(jwksURL), truncate(err.Error())),
		}
	}
	// A JSON null entry unmarshals to a non-empty RawMessage, so filter
	// null/blank entries before counting keys.
	keys := 0
	for _, raw := range jwks.Keys {
		if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
			keys++
		}
	}
	if keys == 0 {
		return Result{
			OK:      false,
			Reason:  ReasonJWKSUnavailable,
			Message: fmt.Sprintf("OIDC signing keys from %s contain no keys", sanitizeURL(jwksURL)),
		}
	}

	return Result{
		OK:      true,
		Reason:  ReasonAvailable,
		Message: fmt.Sprintf("OIDC issuer %s is reachable", sanitizeURL(issuerURL)),
	}
}

// fetch GETs url and returns the body, or a Result describing the failure.
// Only HTTP 200 is accepted; any other status carries the HTTP status in
// Message with an empty Reason, and transport errors carry a full
// classification.
func fetch(ctx context.Context, client *http.Client, url string) ([]byte, Result) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, Result{OK: false, Reason: ReasonIssuerUnreachable, Message: truncate(err.Error())}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Result{OK: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, Result{OK: false, Reason: ReasonIssuerUnreachable, Message: truncate(err.Error())}
	}
	return body, Result{OK: true}
}

// classifyTransportError maps a request failure to IssuerUnreachable or
// TLSUntrusted with an actionable message. The request URL (which may carry a
// query string with credentials) is stripped by unwrapping *url.Error first.
func classifyTransportError(err error) Result {
	errText := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		errText = urlErr.Err.Error()
	}
	if isTLSError(err) {
		return Result{
			OK:     false,
			Reason: ReasonTLSUntrusted,
			Message: fmt.Sprintf(
				"OIDC issuer TLS certificate is not trusted (%s). Add the issuer CA to the DSCInitialization spec.trustedCABundle.customCABundle.",
				truncate(errText),
			),
		}
	}
	return Result{
		OK:      false,
		Reason:  ReasonIssuerUnreachable,
		Message: fmt.Sprintf("Cannot reach OIDC issuer: %s", truncate(errText)),
	}
}

func isTLSError(err error) bool {
	var certVerifyErr *tls.CertificateVerificationError
	if errors.As(err, &certVerifyErr) {
		return true
	}
	var unknownAuthorityErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}
	var certInvalidErr x509.CertificateInvalidError
	return errors.As(err, &certInvalidErr)
}

// resolveReference resolves a possibly-relative jwks_uri against the issuer.
//
// A jwks_uri on a different host than the issuer is legitimate OIDC (many
// providers serve JWKS from a CDN), so the destination is deliberately not
// restricted to the issuer host. The trust boundary is the admin-chosen,
// HTTPS-validated issuerUrl: whoever controls the issuer controls the
// discovery document, exactly as for the production Authorino token
// validation. Enforcing a full egress policy on the probe (DNS-resolved
// address blocking and the like) remains the platform's responsibility,
// mirroring the Authorino HTTP client; the probe only refuses the obvious
// internal-address cases via rejectPrivateAddress.
func resolveReference(base *url.URL, jwksURI string) (string, error) {
	ref, err := url.Parse(jwksURI)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// rejectPrivateAddress reports whether the URL's host is an obvious private,
// loopback, link-local or unspecified IP literal. DNS names are deliberately
// not resolved: this is cheap hygiene against a discovery document naming an
// internal address, not a complete egress policy.
func rejectPrivateAddress(u *url.URL) bool {
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// sanitizeURL strips userinfo, the query string and the fragment so tokens or
// credentials accidentally placed in a URL are never surfaced in condition
// messages.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return truncate(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return truncate(u.String())
}

// truncate caps s at maxMessageLen characters.
func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen] + "…"
}
