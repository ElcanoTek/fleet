package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("read key: %v", err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestSealOpenRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	aad := AAD("fleet:test:v1", "brad@elcano.com", "https://mcp.example.com")
	plaintext := []byte("super-secret-refresh-token")

	sealed, err := c.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("ciphertext contains plaintext — not encrypted")
	}
	if sealed[0] != formatVersion {
		t.Fatalf("version byte = %d, want %d", sealed[0], formatVersion)
	}

	got, err := c.Open(sealed, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open = %q, want %q", got, plaintext)
	}
}

func TestNonceIsRandomPerSeal(t *testing.T) {
	c := newTestCipher(t)
	aad := AAD("p")
	a, _ := c.Seal([]byte("x"), aad)
	b, _ := c.Seal([]byte("x"), aad)
	if bytes.Equal(a, b) {
		t.Fatal("two Seals of the same plaintext produced identical ciphertext — nonce reuse")
	}
}

func TestOpenWrongAADFails(t *testing.T) {
	c := newTestCipher(t)
	sealed, err := c.Seal([]byte("tok"), AAD("fleet:mcp", "userA@x.com", "https://a.example.com"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Swap user — the cross-user attack the AAD binding defends against.
	if _, err := c.Open(sealed, AAD("fleet:mcp", "userB@x.com", "https://a.example.com")); err == nil {
		t.Fatal("Open succeeded with a different user's AAD — cross-user swap not prevented")
	}
	// Swap server URL.
	if _, err := c.Open(sealed, AAD("fleet:mcp", "userA@x.com", "https://b.example.com")); err == nil {
		t.Fatal("Open succeeded with a different server AAD — cross-server swap not prevented")
	}
	// Right AAD still works.
	if _, err := c.Open(sealed, AAD("fleet:mcp", "userA@x.com", "https://a.example.com")); err != nil {
		t.Fatalf("Open with correct AAD failed: %v", err)
	}
}

func TestOpenTamperedCiphertextFails(t *testing.T) {
	c := newTestCipher(t)
	aad := AAD("p")
	sealed, _ := c.Seal([]byte("hello world"), aad)
	// Flip a bit in the ciphertext body (after version+nonce).
	sealed[len(sealed)-1] ^= 0x01
	if _, err := c.Open(sealed, aad); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	c1 := newTestCipher(t)
	c2 := newTestCipher(t)
	aad := AAD("p")
	sealed, _ := c1.Seal([]byte("data"), aad)
	if _, err := c2.Open(sealed, aad); err == nil {
		t.Fatal("Open with a different key succeeded")
	}
}

func TestOpenRejectsShortAndBadVersion(t *testing.T) {
	c := newTestCipher(t)
	if _, err := c.Open([]byte{}, nil); err == nil {
		t.Fatal("Open accepted empty input")
	}
	if _, err := c.Open([]byte{0x01, 0x02}, nil); err == nil {
		t.Fatal("Open accepted too-short input")
	}
	sealed, _ := c.Seal([]byte("x"), nil)
	sealed[0] = 0x99 // unknown version
	if _, err := c.Open(sealed, nil); err == nil {
		t.Fatal("Open accepted unknown version byte")
	}
}

func TestNilCipherFailsClosed(t *testing.T) {
	var c *Cipher
	if _, err := c.Seal([]byte("x"), nil); !errors.Is(err, ErrNoCipher) {
		t.Fatalf("nil Seal err = %v, want ErrNoCipher", err)
	}
	if _, err := c.Open([]byte("x"), nil); !errors.Is(err, ErrNoCipher) {
		t.Fatalf("nil Open err = %v, want ErrNoCipher", err)
	}
}

func TestNewCipherRejectsWrongLength(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("NewCipher accepted a 16-byte key")
	}
}

func TestKeyFromBase64(t *testing.T) {
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("read key: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString(key)
	got, err := KeyFromBase64(enc)
	if err != nil {
		t.Fatalf("KeyFromBase64: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("round-trip mismatch")
	}
	if _, err := KeyFromBase64("not!base64!"); err == nil {
		t.Fatal("accepted invalid base64")
	}
	if _, err := KeyFromBase64(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("accepted wrong-length key")
	}
}

func TestAADLengthPrefixingAvoidsCollision(t *testing.T) {
	// The whole point of length-prefixing: ("a","bc") must not equal ("ab","c").
	if bytes.Equal(AAD("a", "bc"), AAD("ab", "c")) {
		t.Fatal("AAD framing collides on boundary shift")
	}
	if !bytes.Equal(AAD("x", "y"), AAD("x", "y")) {
		t.Fatal("AAD not deterministic")
	}
}
