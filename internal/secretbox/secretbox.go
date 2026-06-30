// Package secretbox is a tiny authenticated-encryption helper for secrets that
// must live at rest in Postgres rather than the 0600 env file — today the
// per-user remote-MCP OAuth tokens (#443).
//
// It is AES-256-GCM with a caller-supplied AAD (Additional Authenticated Data),
// laid out as:
//
//	version(1) || nonce(12) || ciphertext||tag
//
// The version byte is a forward-compat hook: a future key rotation can write
// v2 ciphertext while still decrypting v1, without a migration flag day. The
// nonce is fresh CSPRNG bytes per Seal (never reused under one key).
//
// SECURITY — why the AAD matters. The token columns are encrypted with an AAD
// bound to (purpose, user-email, canonical-server-URI). GCM authenticates the
// AAD without storing it, so a ciphertext lifted from one row and pasted into
// another (a different user, or the same user's different server) fails to
// open rather than silently decrypting and authorizing as the wrong
// principal. The AAD must therefore be reconstructed byte-identically at Open
// time from the row's own (email, canonical URL) — see [AAD].
//
// The key lives in an env var on the same host as the ciphertext, so this is
// defense-in-depth against a leaked DB backup / SQL-injection read, NOT against
// full host compromise (an attacker with the host gets both key and data). That
// matches fleet's existing at-rest posture (the env-file credential store, the
// log-archive key). The version byte leaves the door open to envelope/KMS
// encryption later.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeyLen is the required AES-256 key length in bytes.
const KeyLen = 32

// formatVersion is the leading byte of every ciphertext. Bump it (and keep the
// old branch in Open) to rotate the key format without re-encrypting old rows
// in a single migration.
const formatVersion byte = 1

// ErrNoCipher is returned by [Cipher] methods on a nil cipher — the fail-closed
// signal that the feature's encryption key is unset. Callers surface it to the
// operator ("set FLEET_MCP_OAUTH_ENCRYPTION_KEY") rather than storing plaintext.
var ErrNoCipher = errors.New("secretbox: no encryption key configured")

// Cipher seals and opens secrets under a single AES-256 key. The zero value is
// not usable; construct with [NewCipher]. A nil *Cipher is a valid "feature
// disabled" sentinel: its methods return [ErrNoCipher] rather than panicking,
// so a caller can hold an optional *Cipher and let the nil case fail closed.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte AES-256 key. A wrong-length key is a
// configuration error, not a runtime fallback — the caller decides what to do
// (typically: disable the feature and hold a nil *Cipher).
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("secretbox: key must be %d bytes (got %d)", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// KeyFromBase64 decodes a standard-base64 key and validates its length. Used to
// turn the FLEET_MCP_OAUTH_ENCRYPTION_KEY env value into key bytes. The decoded
// key is never logged by this package.
func KeyFromBase64(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("secretbox: key is not valid base64: %w", err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("secretbox: key must decode to %d bytes (got %d)", KeyLen, len(key))
	}
	return key, nil
}

// Seal encrypts plaintext, authenticating aad without storing it. The result is
// version || nonce || ciphertext||tag. A nil Cipher returns [ErrNoCipher].
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	if c == nil {
		return nil, ErrNoCipher
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secretbox: generate nonce: %w", err)
	}
	// Prefix the version byte, then let Seal append ciphertext+tag onto the
	// version||nonce header so the whole record is one allocation.
	header := make([]byte, 1+len(nonce))
	header[0] = formatVersion
	copy(header[1:], nonce)
	return c.aead.Seal(header, nonce, plaintext, aad), nil
}

// Open reverses Seal. aad MUST be byte-identical to the value passed to Seal or
// authentication fails. The error never echoes key or ciphertext material —
// only that authentication failed — so it is safe to log.
func (c *Cipher) Open(sealed, aad []byte) ([]byte, error) {
	if c == nil {
		return nil, ErrNoCipher
	}
	nonceSize := c.aead.NonceSize()
	// 1 version byte + nonce + at least the GCM tag.
	if len(sealed) < 1+nonceSize+c.aead.Overhead() {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	if sealed[0] != formatVersion {
		return nil, fmt.Errorf("secretbox: unknown ciphertext version %d", sealed[0])
	}
	nonce := sealed[1 : 1+nonceSize]
	ciphertext := sealed[1+nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("secretbox: decryption failed: authentication error")
	}
	return plaintext, nil
}

// AAD builds a canonical, collision-free Additional-Authenticated-Data blob
// from its parts using length-prefixed framing, so that AAD("a","bc") differs
// from AAD("ab","c") (naive concatenation would collide them). Every part is
// length-prefixed with a 4-byte big-endian count. Callers pass a stable
// purpose string first, then the binding identity (e.g. lowercased email and
// the canonical server URI) — reconstructed identically at Open time.
func AAD(parts ...string) []byte {
	total := 0
	for _, p := range parts {
		total += 4 + len(p)
	}
	out := make([]byte, 0, total)
	var lenbuf [4]byte
	for _, p := range parts {
		//nolint:gosec // G115: AAD parts are short identifiers (purpose label, email, URL), never near 4 GiB.
		binary.BigEndian.PutUint32(lenbuf[:], uint32(len(p)))
		out = append(out, lenbuf[:]...)
		out = append(out, p...)
	}
	return out
}
