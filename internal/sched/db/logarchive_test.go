package db

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

// TestEncodeDecodeArchiveGzip exercises the gzip-only compress -> decompress
// roundtrip and asserts compression actually shrinks a realistic (repetitive)
// log payload.
func TestEncodeDecodeArchiveGzip(t *testing.T) {
	raw := []byte(strings.Repeat(`{"role":"assistant","content":"thinking about the task"}`, 200))

	stored, codec, err := encodeArchive(raw, nil)
	if err != nil {
		t.Fatalf("encodeArchive: %v", err)
	}
	if codec != compressionGzip {
		t.Fatalf("codec = %q, want %q", codec, compressionGzip)
	}
	if len(stored) >= len(raw) {
		t.Fatalf("gzip did not shrink payload: stored=%d raw=%d", len(stored), len(raw))
	}

	got, err := decodeArchive(stored, nil, codec)
	if err != nil {
		t.Fatalf("decodeArchive: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(got), len(raw))
	}
}

// TestEncodeDecodeArchiveEncrypted exercises the gzip+AES-256-GCM roundtrip and
// confirms the stored bytes neither equal the plaintext nor leak it.
func TestEncodeDecodeArchiveEncrypted(t *testing.T) {
	key := make([]byte, aesKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	raw := []byte(strings.Repeat(`{"role":"user","content":"secret prompt"}`, 50))

	stored, codec, err := encodeArchive(raw, key)
	if err != nil {
		t.Fatalf("encodeArchive: %v", err)
	}
	if codec != compressionGzipAES {
		t.Fatalf("codec = %q, want %q", codec, compressionGzipAES)
	}
	if bytes.Contains(stored, []byte("secret prompt")) {
		t.Fatal("ciphertext leaks plaintext")
	}

	got, err := decodeArchive(stored, key, codec)
	if err != nil {
		t.Fatalf("decodeArchive: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("encrypted roundtrip mismatch")
	}
}

// TestDecodeArchiveWrongKey confirms a tampered/incorrect key fails closed (GCM
// authentication) rather than returning garbage.
func TestDecodeArchiveWrongKey(t *testing.T) {
	key := make([]byte, aesKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	stored, codec, err := encodeArchive([]byte(`{"x":1}`), key)
	if err != nil {
		t.Fatalf("encodeArchive: %v", err)
	}

	wrong := make([]byte, aesKeyLen) // all-zero, different key
	if _, err := decodeArchive(stored, wrong, codec); err == nil {
		t.Fatal("decodeArchive succeeded with the wrong key; want authentication failure")
	}
}

// TestDecodeArchiveMissingKey confirms reading an encrypted archive without a
// configured key surfaces ErrLogArchiveKeyMissing.
func TestDecodeArchiveMissingKey(t *testing.T) {
	key := make([]byte, aesKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	stored, codec, err := encodeArchive([]byte(`{"x":1}`), key)
	if err != nil {
		t.Fatalf("encodeArchive: %v", err)
	}
	if _, err := decodeArchive(stored, nil, codec); !errors.Is(err, ErrLogArchiveKeyMissing) {
		t.Fatalf("err = %v, want ErrLogArchiveKeyMissing", err)
	}
}

// TestNewGCMRejectsWrongKeyLength guards the AES-256 key-length contract.
func TestNewGCMRejectsWrongKeyLength(t *testing.T) {
	if _, err := newGCM(make([]byte, 16)); err == nil {
		t.Fatal("newGCM accepted a 16-byte key; want rejection")
	}
}

// TestDecodeArchiveUnknownCodec guards against a corrupt/unknown marker.
func TestDecodeArchiveUnknownCodec(t *testing.T) {
	if _, err := decodeArchive([]byte("x"), nil, "lz4"); err == nil {
		t.Fatal("decodeArchive accepted an unknown codec")
	}
}
