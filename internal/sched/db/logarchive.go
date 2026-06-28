package db

// Log-archival codec (#272). Old terminal-task log payloads are compressed (and
// optionally AES-256-GCM encrypted) IN PLACE: session_data (the live plaintext
// JSONB) is nulled and the bytes move into session_data_gz (BYTEA), with
// session_compression naming the codec so GetLog can transparently inflate them
// on read. This file holds the pure (DB-free) encode/decode helpers; the sweep
// and the read paths live in db.go.
//
// The optional encryption key is held host-side on the Database (SetLogArchiveKey)
// and is NEVER logged or returned in errors — consistent with the project's
// credentials-stay-host-side invariant. Errors reference the codec name only.

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Compression markers stored in logs.session_compression. An empty/NULL marker
// means the payload is live plaintext JSON in session_data (not archived).
const (
	compressionGzip    = "gzip"           // gzip only
	compressionGzipAES = "gzip+aes256gcm" // gzip then AES-256-GCM
)

// aesKeyLen is the AES-256 key length in bytes.
const aesKeyLen = 32

// ErrLogArchiveKeyMissing is returned when an AES-encrypted archive must be read
// but no archive key is configured on the Database.
var ErrLogArchiveKeyMissing = errors.New("encrypted log archive requires an archive key but none is configured")

// encodeArchive gzip-compresses raw, optionally AES-256-GCM encrypting the
// result when key is non-nil. It returns the stored bytes and the codec marker
// to record alongside them.
func encodeArchive(raw, key []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, "", fmt.Errorf("gzip log payload: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, "", fmt.Errorf("gzip log payload: %w", err)
	}
	if len(key) == 0 {
		return buf.Bytes(), compressionGzip, nil
	}
	enc, err := encryptGCM(key, buf.Bytes())
	if err != nil {
		return nil, "", err
	}
	return enc, compressionGzipAES, nil
}

// decodeArchive reverses encodeArchive: it decrypts (when the codec calls for it)
// then gunzips stored back to the original JSON bytes. key may be nil for the
// plain-gzip codec.
func decodeArchive(stored, key []byte, codec string) ([]byte, error) {
	gzipped := stored
	switch codec {
	case compressionGzip:
		// no decryption
	case compressionGzipAES:
		if len(key) == 0 {
			return nil, ErrLogArchiveKeyMissing
		}
		plain, err := decryptGCM(key, stored)
		if err != nil {
			return nil, err
		}
		gzipped = plain
	default:
		return nil, fmt.Errorf("unknown log compression codec %q", codec)
	}
	gz, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		return nil, fmt.Errorf("open gzip log payload: %w", err)
	}
	defer func() { _ = gz.Close() }()
	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("inflate gzip log payload: %w", err)
	}
	return raw, nil
}

// encryptGCM AES-256-GCM encrypts plaintext under key, prepending the random
// 12-byte nonce to the ciphertext (the standard seal-with-nonce-prefix layout).
func encryptGCM(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate archive nonce: %w", err)
	}
	// Seal appends the ciphertext+tag to its first arg (the nonce), yielding
	// nonce||ciphertext||tag in one slice.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decryptGCM reverses encryptGCM. The error text never echoes the ciphertext or
// key — only that authentication failed.
func decryptGCM(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(sealed) < nonceSize {
		return nil, errors.New("archive ciphertext too short")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("decrypt log archive: authentication failed")
	}
	return plaintext, nil
}

// newGCM builds an AES-256-GCM AEAD from a 32-byte key.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != aesKeyLen {
		return nil, fmt.Errorf("archive key must be %d bytes (got %d)", aesKeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return gcm, nil
}
