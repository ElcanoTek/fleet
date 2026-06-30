package mcp

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// TLSOptions hardens the TLS handshake for ONE remote (HTTP) MCP server (#280).
// Every field is optional; the zero value means "use Go's default transport
// security" (system root CAs, no client certificate, no pinning) — exactly the
// behavior before this type existed. The fields compose:
//
//   - CACertFile pins TRUST to a specific CA bundle (PEM). Use it for an
//     internal/self-signed MCP server: the server's chain must verify against
//     THIS pool instead of the system roots.
//   - ClientCertFile + ClientKeyFile present a client certificate (mTLS), so the
//     MCP server can require that only fleet connects. Both or neither.
//   - PinnedSHA256 pins the server's leaf public key: a hex SHA-256 of the cert's
//     SubjectPublicKeyInfo. It is checked IN ADDITION to normal chain
//     verification (defense in depth), so a substituted certificate is rejected
//     even if it chains to an otherwise-trusted (e.g. rogue/compromised) CA. The
//     pin survives certificate renewal as long as the key is reused. Compute it
//     with: openssl x509 -in cert.pem -pubkey -noout |
//     openssl pkey -pubin -outform der | openssl dgst -sha256
//   - ServerName overrides the SNI / verified hostname (useful when pinning a
//     server reachable under an address that differs from its cert's name).
//
// Cert/key files are operator-supplied paths on the fleet host; they are read at
// connect time and never enter the sandbox or the model context.
type TLSOptions struct {
	CACertFile     string
	ClientCertFile string
	ClientKeyFile  string
	PinnedSHA256   string
	ServerName     string
}

// IsZero reports whether no TLS hardening was requested (every field empty).
func (o TLSOptions) IsZero() bool { return o == TLSOptions{} }

// build turns the options into a *tls.Config, or (nil, nil) when nothing was
// requested (the caller then keeps the default client). It returns a clear error
// — never a silently-insecure config — when a file can't be read/parsed or the
// pin is malformed. It NEVER sets InsecureSkipVerify: pinning is layered on top
// of normal verification, and a self-signed server is trusted by supplying its
// CA via CACertFile, not by disabling verification.
func (o TLSOptions) build() (*tls.Config, error) {
	if o.IsZero() {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if o.ServerName != "" {
		cfg.ServerName = o.ServerName
	}

	if o.CACertFile != "" {
		pem, err := os.ReadFile(o.CACertFile) //nolint:gosec // G304: operator-supplied CA bundle path, a deliberate local file.
		if err != nil {
			return nil, fmt.Errorf("read ca_cert %q: %w", o.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_cert %q: no PEM certificates found", o.CACertFile)
		}
		cfg.RootCAs = pool
	}

	if o.ClientCertFile != "" || o.ClientKeyFile != "" {
		if o.ClientCertFile == "" || o.ClientKeyFile == "" {
			return nil, fmt.Errorf("mTLS requires both client_cert and client_key (got cert=%q key=%q)", o.ClientCertFile, o.ClientKeyFile)
		}
		cert, err := tls.LoadX509KeyPair(o.ClientCertFile, o.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	if o.PinnedSHA256 != "" {
		pin, err := NormalizePinSHA256(o.PinnedSHA256)
		if err != nil {
			return nil, err
		}
		// VerifyConnection runs AFTER the standard chain verification, so the pin
		// is an additional gate, not a replacement for it.
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("tls pin: server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			got := hex.EncodeToString(sum[:])
			if got != pin {
				return fmt.Errorf("tls pin mismatch: server public-key SHA-256 %s does not match pinned_sha256", got)
			}
			return nil
		}
	}

	return cfg, nil
}

// NormalizePinSHA256 canonicalizes a configured public-key pin to lowercase hex.
// It tolerates an optional "sha256:" prefix and ':'/whitespace separators, and
// requires exactly 32 bytes (64 hex chars) — anything else is a config error.
func NormalizePinSHA256(pin string) (string, error) {
	p := strings.TrimSpace(pin)
	p = strings.TrimPrefix(strings.ToLower(p), "sha256:")
	p = strings.NewReplacer(":", "", " ", "", "\t", "").Replace(p)
	p = strings.ToLower(p)
	if len(p) != 64 {
		return "", fmt.Errorf("pinned_sha256 must be a 32-byte (64 hex char) SHA-256, got %d chars", len(p))
	}
	if _, err := hex.DecodeString(p); err != nil {
		return "", fmt.Errorf("pinned_sha256 is not valid hex: %w", err)
	}
	return p, nil
}

// tlsHTTPClient builds an MCP HTTP client that uses cfg for TLS while preserving
// the default transport's connection-pool/proxy/timeout behavior and the bounded
// per-call timeout. A clone of http.DefaultTransport keeps those defaults; only
// the TLS config is swapped.
func tlsHTTPClient(cfg *tls.Config) *http.Client {
	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.TLSClientConfig = cfg
	return &http.Client{
		Timeout:   DefaultMCPHTTPTimeout,
		Transport: tr,
	}
}
