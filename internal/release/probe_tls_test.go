// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// domainTLSServer serves /readyz over TLS with a self-signed certificate that
// names ONLY domain (no IP SAN, as an ACME or operator certificate would be),
// and writes that certificate to a PEM file like FLEET_TLS_CERT_FILE.
func domainTLSServer(t *testing.T, domain string) (url, certFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	certFile = filepath.Join(t.TempDir(), "fleet.crt")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// srv.URL is https://127.0.0.1:<port> — the loopback address the probe dials.
	return srv.URL + "/readyz", certFile
}

func probe(t *testing.T, url string, cfg *tls.Config) error {
	t.Helper()
	return HTTPProber{TLS: cfg, Interval: 10 * time.Millisecond}.WaitReady(t.Context(), url, 300*time.Millisecond)
}

func TestProbeVerifiesTheDomainOverLoopback(t *testing.T) {
	url, certFile := domainTLSServer(t, "fleet.example.test")
	if !strings.HasPrefix(url, "https://127.0.0.1:") {
		t.Fatalf("fixture must be probed via loopback, got %s", url)
	}
	cfg, err := ProbeTLSConfig(url, "manual", "fleet.example.test", certFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("verification must never be switched off")
	}
	if err := probe(t, url, cfg); err != nil {
		t.Fatalf("the right domain + the configured certificate must verify: %v", err)
	}
}

func TestProbeRefusesTheWrongDomain(t *testing.T) {
	url, certFile := domainTLSServer(t, "fleet.example.test")
	cfg, err := ProbeTLSConfig(url, "manual", "other.example.test", certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe(t, url, cfg); err == nil {
		t.Fatal("a certificate for another domain passed the probe")
	}
	// And the reason is the certificate, not a timeout.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a certificate for another domain verified")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("want a certificate verification error, got %v", err)
	}
}

func TestProbeUsesTheCertFileOnlyInManualMode(t *testing.T) {
	url, certFile := domainTLSServer(t, "fleet.example.test")
	// auto mode: system roots only (an ACME chain verifies there); a
	// self-signed certificate does not, and must not be waved through.
	cfg, err := ProbeTLSConfig(url, "auto", "fleet.example.test", certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe(t, url, cfg); err == nil {
		t.Fatal("auto mode trusted a certificate outside the system roots")
	}
	// Without verification material the default client also refuses.
	if err := probe(t, url, nil); err == nil {
		t.Fatal("an unverified loopback certificate was accepted with no TLS config")
	}
}

func TestProbeTLSConfigNeedsADomain(t *testing.T) {
	if _, err := ProbeTLSConfig("https://127.0.0.1:8443/readyz", "auto", " ", ""); !errors.Is(err, ErrProbeServerName) {
		t.Fatalf("want ErrProbeServerName with no domain, got %v", err)
	}
	for _, u := range []string{"http://127.0.0.1:8080/readyz", "https://fleet.example.com/readyz"} {
		if cfg, err := ProbeTLSConfig(u, "manual", "", ""); err != nil || cfg != nil {
			t.Errorf("%s: want no special TLS config, got %v %v", u, cfg, err)
		}
	}
	if _, err := ProbeTLSConfig("https://127.0.0.1:8443/readyz", "manual", "fleet.example.test", filepath.Join(t.TempDir(), "missing.crt")); err == nil {
		t.Error("an unreadable FLEET_TLS_CERT_FILE must be an error, not silently ignored")
	}
}
