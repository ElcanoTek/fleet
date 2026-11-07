package otelsetup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCertPair writes a self-signed certificate and its key as PEM files and
// returns their paths.
func writeCertPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestSplitEndpoint(t *testing.T) {
	for _, tc := range []struct{ in, host, hostport string }{
		{"collector:4317", "collector", "collector:4317"},
		{"https://collector.example.com:4317", "collector.example.com", "collector.example.com:4317"},
		{"grpc://127.0.0.1:4317", "127.0.0.1", "127.0.0.1:4317"},
		{"[::1]:4317", "::1", "[::1]:4317"},
	} {
		host, hostport, err := splitEndpoint(tc.in)
		if err != nil || host != tc.host || hostport != tc.hostport {
			t.Errorf("splitEndpoint(%q) = (%q, %q, %v), want (%q, %q)", tc.in, host, hostport, err, tc.host, tc.hostport)
		}
	}
	for _, bad := range []string{"collector", "https://", "http://:"} {
		if _, _, err := splitEndpoint(bad); err == nil {
			t.Errorf("splitEndpoint(%q) accepted an endpoint without host:port", bad)
		}
	}
}

// Plaintext is allowed only when asked for AND loopback; a plaintext request
// for a remote collector is refused, as is a contradictory configuration.
func TestInsecureOnlyForExplicitLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:4317", "localhost:4317", "[::1]:4317"} {
		if _, err := (ExporterConfig{Endpoint: ok, Insecure: true}).grpcOptions(); err != nil {
			t.Errorf("insecure loopback %s refused: %v", ok, err)
		}
	}
	for _, remote := range []string{"collector:4317", "10.0.0.5:4317", "https://otel.example.com:4317"} {
		_, err := (ExporterConfig{Endpoint: remote, Insecure: true}).grpcOptions()
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("insecure %s: err = %v, want a loopback refusal", remote, err)
		}
	}
	if _, err := (ExporterConfig{Endpoint: "127.0.0.1:4317", Insecure: true, CAFile: "/x"}).grpcOptions(); err == nil {
		t.Error("insecure + a CA file should be refused as contradictory")
	}
	// Without the switch, a loopback endpoint is TLS like any other.
	if (ExporterConfig{Endpoint: "127.0.0.1:4317"}).transport() != "tls" {
		t.Error("the default transport must be TLS")
	}
}

func TestTLSConfig(t *testing.T) {
	dir := t.TempDir()
	caCert, _ := writeCertPair(t, dir, "ca")
	clientCert, clientKey := writeCertPair(t, dir, "client")

	cfg, err := (ExporterConfig{Endpoint: "collector:4317", CAFile: caCert}).tlsConfig("collector")
	if err != nil {
		t.Fatalf("CA-only TLS: %v", err)
	}
	if cfg.RootCAs == nil || len(cfg.Certificates) != 0 || cfg.ServerName != "collector" || cfg.MinVersion < 0x0303 {
		t.Errorf("CA-only TLS config = %+v", cfg)
	}

	mtls := ExporterConfig{Endpoint: "collector:4317", CAFile: caCert, ClientCertFile: clientCert, ClientKeyFile: clientKey}
	cfg, err = mtls.tlsConfig("collector")
	if err != nil {
		t.Fatalf("mTLS: %v", err)
	}
	if len(cfg.Certificates) != 1 || mtls.transport() != "mtls" {
		t.Errorf("mTLS config carries %d client certs (transport %s)", len(cfg.Certificates), mtls.transport())
	}

	if _, err := (ExporterConfig{ClientCertFile: clientCert}).tlsConfig("c"); err == nil {
		t.Error("half a client key pair should be refused")
	}
	notPEM := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	if _, err := (ExporterConfig{CAFile: notPEM}).tlsConfig("c"); err == nil {
		t.Error("a CA file with no PEM certificate should be refused")
	}
	if _, err := (ExporterConfig{CAFile: filepath.Join(dir, "missing.pem")}).tlsConfig("c"); err == nil {
		t.Error("a missing CA file should be refused")
	}
}

// A misconfigured exporter fails Init (the caller logs and runs untraced)
// rather than exporting in plaintext to a remote host.
func TestInitRefusesRemotePlaintext(t *testing.T) {
	t.Setenv("FLEET_OTEL_ENDPOINT", "collector.example.com:4317")
	t.Setenv("FLEET_OTEL_INSECURE", "true")
	if _, err := Init(context.Background(), "test"); err == nil {
		t.Fatal("Init exported plaintext to a remote collector")
	}
}

// A TLS exporter to a configured endpoint initializes (the gRPC dial is lazy,
// so no collector needs to be listening) and shuts down cleanly.
func TestInitTLSExporter(t *testing.T) {
	dir := t.TempDir()
	caCert, _ := writeCertPair(t, dir, "ca")
	t.Setenv("FLEET_OTEL_ENDPOINT", "127.0.0.1:1")
	t.Setenv("FLEET_OTEL_INSECURE", "")
	t.Setenv("FLEET_OTEL_CA_FILE", caCert)
	shutdown, err := InitWith(context.Background(), Options{ServiceVersion: "test", Process: "server"})
	if err != nil {
		t.Fatalf("InitWith: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
