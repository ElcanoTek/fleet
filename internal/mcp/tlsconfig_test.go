package mcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizePinSHA256(t *testing.T) {
	valid := "aa" + strings.Repeat("bb", 31) // 64 hex chars

	// Accepted forms all normalize to the same lowercase, separator-free hex.
	for _, in := range []string{
		valid,
		strings.ToUpper(valid),             // case-folded
		"sha256:" + valid,                  // prefix stripped
		"SHA256:" + strings.ToUpper(valid), // prefix + case
		"aa " + strings.Repeat("bb", 31),   // whitespace stripped
	} {
		got, err := NormalizePinSHA256(in)
		if err != nil || got != valid {
			t.Errorf("NormalizePinSHA256(%q) = %q, %v; want %q", in, got, err, valid)
		}
	}
	// Colon-separated form normalizes to plain hex too.
	colon := strings.Join(splitN(valid, 2), ":")
	if got, err := NormalizePinSHA256(colon); err != nil || got != valid {
		t.Errorf("colon form: got %q err %v, want %q", got, err, valid)
	}
	// Rejected: wrong length, non-hex, empty.
	for _, bad := range []string{"deadbeef", "zz" + strings.Repeat("bb", 31), ""} {
		if _, err := NormalizePinSHA256(bad); err == nil {
			t.Errorf("NormalizePinSHA256(%q): want error", bad)
		}
	}
}

func splitN(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func TestTLSOptionsBuild(t *testing.T) {
	certPath, keyPath, cert := genSelfSigned(t)

	t.Run("zero options → nil config", func(t *testing.T) {
		cfg, err := TLSOptions{}.build()
		if err != nil || cfg != nil {
			t.Fatalf("zero options: cfg=%v err=%v, want nil,nil", cfg, err)
		}
	})

	t.Run("mTLS requires both cert and key", func(t *testing.T) {
		if _, err := (TLSOptions{ClientCertFile: certPath}).build(); err == nil {
			t.Error("client_cert without client_key should error")
		}
		if _, err := (TLSOptions{ClientKeyFile: keyPath}).build(); err == nil {
			t.Error("client_key without client_cert should error")
		}
	})

	t.Run("valid CA file populates RootCAs", func(t *testing.T) {
		cfg, err := (TLSOptions{CACertFile: certPath}).build()
		if err != nil || cfg == nil || cfg.RootCAs == nil {
			t.Fatalf("CA file: cfg=%v err=%v", cfg, err)
		}
	})

	t.Run("missing CA file errors", func(t *testing.T) {
		if _, err := (TLSOptions{CACertFile: filepath.Join(t.TempDir(), "nope.pem")}).build(); err == nil {
			t.Error("missing ca_cert should error")
		}
	})

	t.Run("valid client keypair populates Certificates", func(t *testing.T) {
		cfg, err := (TLSOptions{ClientCertFile: certPath, ClientKeyFile: keyPath}).build()
		if err != nil || cfg == nil || len(cfg.Certificates) != 1 {
			t.Fatalf("client keypair: cfg=%v err=%v", cfg, err)
		}
	})

	t.Run("bad pin errors", func(t *testing.T) {
		if _, err := (TLSOptions{PinnedSHA256: "nothex"}).build(); err == nil {
			t.Error("bad pin should error")
		}
	})

	t.Run("valid pin sets VerifyConnection", func(t *testing.T) {
		pin := spkiPin(cert)
		cfg, err := (TLSOptions{PinnedSHA256: pin}).build()
		if err != nil || cfg == nil || cfg.VerifyConnection == nil {
			t.Fatalf("pin: cfg=%v err=%v", cfg, err)
		}
	})
}

// TestAddHTTPServerWithOptions_TLS exercises the full path: the TLS config built
// from TLSOptions must actually reach the transport and be enforced against a
// real TLS handshake.
func TestAddHTTPServerWithOptions_TLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(fakeMCPHandler))
	defer ts.Close()

	caPath := writeCertPEM(t, ts.Certificate())
	goodPin := spkiPin(ts.Certificate())

	t.Run("CA pin + correct SPKI pin connects", func(t *testing.T) {
		c := NewClient()
		defer c.Close()
		err := c.AddHTTPServerWithOptions(context.Background(), "t", ts.URL,
			HTTPServerOptions{TLS: &TLSOptions{CACertFile: caPath, PinnedSHA256: goodPin}})
		if err != nil {
			t.Fatalf("expected success with correct CA + pin, got %v", err)
		}
		if len(c.GetAllTools()) != 1 {
			t.Errorf("expected the server's 1 tool to register")
		}
	})

	t.Run("wrong SPKI pin is rejected", func(t *testing.T) {
		c := NewClient()
		defer c.Close()
		wrong := "ab" + strings.Repeat("cd", 31)
		err := c.AddHTTPServerWithOptions(context.Background(), "t", ts.URL,
			HTTPServerOptions{TLS: &TLSOptions{CACertFile: caPath, PinnedSHA256: wrong}})
		if err == nil {
			t.Fatal("expected pin mismatch to fail the connection")
		}
	})

	t.Run("self-signed server is rejected without a CA (verification not disabled)", func(t *testing.T) {
		c := NewClient()
		defer c.Close()
		// Only a pin, no CA: normal chain verification still runs and must reject
		// the untrusted self-signed cert — proving we never set InsecureSkipVerify.
		err := c.AddHTTPServerWithOptions(context.Background(), "t", ts.URL,
			HTTPServerOptions{TLS: &TLSOptions{PinnedSHA256: goodPin}})
		if err == nil {
			t.Fatal("expected an untrusted self-signed cert to be rejected")
		}
	})

	t.Run("TLS hardening on a plaintext http url is refused (fail closed)", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(fakeMCPHandler))
		defer plain.Close()
		c := NewClient()
		defer c.Close()
		// http.Transport would silently NOT apply the TLS config to an http url;
		// registration must refuse rather than connect unverified.
		err := c.AddHTTPServerWithOptions(context.Background(), "t", plain.URL,
			HTTPServerOptions{TLS: &TLSOptions{CACertFile: caPath, PinnedSHA256: goodPin}})
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("expected refusal of TLS hardening over plaintext http, got %v", err)
		}
	})
}

// fakeMCPHandler answers just enough JSON-RPC for server registration.
func fakeMCPHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		resp["result"] = map[string]interface{}{"protocolVersion": "2024-11-05"}
	case "tools/list":
		resp["result"] = map[string]interface{}{"tools": []Tool{{Name: "remote_echo", Description: "Remote Echo"}}}
	default:
		resp["error"] = map[string]interface{}{"code": -32601, "message": "Method not found"}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func spkiPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func writeCertPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// genSelfSigned writes a self-signed ECDSA cert + key to temp PEM files and
// returns their paths and the parsed certificate.
func genSelfSigned(t *testing.T) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31-1, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, cert
}
