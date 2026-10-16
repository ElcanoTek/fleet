package webpush

// The Web Push wire protocol, on the standard library alone: RFC 8291 message
// encryption (the aes128gcm content coding of RFC 8188, keyed by an ephemeral
// ECDH exchange with the browser's subscription key) and RFC 8292 VAPID (an
// ES256 JWT naming this server to the push service). It replaces
// github.com/SherClockHolmes/webpush-go, whose behaviour it keeps where fleet
// depended on it — see docs/IN-HOUSED-DEPENDENCIES.md:
//
//   - VAPID keys are the same strings: the private key is the base64url
//     P-256 scalar, the public key the base64url uncompressed point, so keys
//     already in an operator's env-file keep working, padded or not.
//   - A subscription's p256dh and auth are read as standard or URL base64,
//     with or without padding, as browsers and older rows store them.
//   - Every message is one 4096-byte record: the plaintext is padded to fill
//     it, so the ciphertext length says nothing about the notification.
//   - The VAPID token is {"alg":"ES256","typ":"JWT"} over {aud, exp, sub}: aud
//     is the endpoint's scheme and host, exp twelve hours ahead, and sub the
//     contact, prefixed "mailto:" unless it starts with "https:".
//
// No deprecated crypto/elliptic arithmetic and no JWT library: crypto/ecdh for
// the key agreement, crypto/hkdf, crypto/aes + crypto/cipher for AES-128-GCM,
// and crypto/ecdsa for the signature.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Urgency is the RFC 8030 Urgency header: how important a message is, which a
// push service may use to decide whether to wake a device on low battery.
type Urgency string

// The four RFC 8030 urgencies.
const (
	UrgencyVeryLow Urgency = "very-low"
	UrgencyLow     Urgency = "low"
	UrgencyNormal  Urgency = "normal"
	UrgencyHigh    Urgency = "high"
)

func (u Urgency) valid() bool {
	switch u {
	case UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh:
		return true
	}
	return false
}

// HTTPClient is the seam a send goes through; *http.Client satisfies it.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

const (
	// recordSize is the aes128gcm record size: every message is one record of
	// exactly this many octets (RFC 8291 §4 allows up to 4096).
	recordSize = 4096
	// gcmTagLen is AES-GCM's authentication tag length.
	gcmTagLen = 16
	// vapidTokenLifetime is how far ahead the VAPID token's exp lies (RFC 8292
	// allows at most 24 hours).
	vapidTokenLifetime = 12 * time.Hour
	// paddingDelimiter ends the plaintext of the last (here: only) record.
	paddingDelimiter = 0x02
)

// errPayloadTooLarge is returned for a payload that does not fit one record.
var errPayloadTooLarge = errors.New("payload has exceeded the maximum length")

// GenerateVAPIDKeys creates a VAPID key pair: the base64url (unpadded) P-256
// private scalar and uncompressed public point — the format FLEET_VAPID_* have
// always held.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	priv, err := key.Bytes()
	if err != nil {
		return "", "", err
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(priv), base64.RawURLEncoding.EncodeToString(pub), nil
}

// pushMessage is one notification to one subscription.
type pushMessage struct {
	Endpoint string
	Auth     string // the subscription's auth secret, base64
	P256dh   string // the subscription's public key, base64
	Payload  []byte
	TTL      int
	Urgency  Urgency
	// VAPID: the server's key pair (as configured) and the contact for sub.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	Subscriber      string
}

// send encrypts m for its subscription, signs a VAPID token, and POSTs it to
// the endpoint through client. The caller owns the response body.
func send(ctx context.Context, client HTTPClient, m pushMessage) (*http.Response, error) {
	authSecret, err := decodeSubscriptionKey(m.Auth)
	if err != nil {
		return nil, fmt.Errorf("subscription auth key: %w", err)
	}
	uaPublic, err := decodeSubscriptionKey(m.P256dh)
	if err != nil {
		return nil, fmt.Errorf("subscription p256dh key: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	body, err := encrypt(m.Payload, authSecret, uaPublic, asPrivate, salt, true)
	if err != nil {
		return nil, err
	}
	authorization, err := vapidAuthorization(m.Endpoint, m.Subscriber, m.VAPIDPublicKey, m.VAPIDPrivateKey, time.Now())
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(m.TTL))
	if m.Urgency.valid() {
		req.Header.Set("Urgency", string(m.Urgency))
	}
	req.Header.Set("Authorization", authorization)
	return client.Do(req)
}

// encrypt is RFC 8291 §3.4 + RFC 8188: derive the content key and nonce from
// the ECDH secret between the ephemeral asPrivate and the browser's uaPublic,
// mixed with its auth secret, and seal the payload as one aes128gcm record
// headed by salt, record size and asPrivate's public key. With fill, the
// plaintext is padded with zeros to fill the 4096-octet record (what fleet
// sends); without, only the delimiter is added (RFC 8291's worked example).
func encrypt(payload, authSecret, uaPublic []byte, asPrivate *ecdh.PrivateKey, salt []byte, fill bool) ([]byte, error) {
	if len(salt) != 16 {
		return nil, errors.New("salt must be 16 octets")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, errors.New("subscription p256dh is not a valid P-256 public key")
	}
	ecdhSecret, err := asPrivate.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := asPrivate.PublicKey().Bytes()

	// key_info = "WebPush: info" || 0x00 || ua_public || as_public
	keyInfo := make([]byte, 0, 14+len(uaPublic)+len(asPublic))
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	// Header: salt (16) || rs (uint32) || idlen (1) || keyid (as_public).
	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPublic))) //nolint:gosec // G115: an uncompressed P-256 point is always 65 octets
	header = append(header, asPublic...)

	plaintext := make([]byte, 0, recordSize)
	plaintext = append(plaintext, payload...)
	plaintext = append(plaintext, paddingDelimiter)
	if fill {
		// The record's plaintext fills what the header and tag leave.
		room := recordSize - gcmTagLen - len(header)
		if len(plaintext) > room {
			return nil, errPayloadTooLarge
		}
		plaintext = append(plaintext, make([]byte, room-len(plaintext))...)
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(header, nonce, plaintext, nil), nil
}

// vapidAuthorization is the RFC 8292 Authorization header value:
// "vapid t=<ES256 JWT>, k=<public key>".
func vapidAuthorization(endpoint, subscriber, publicKey, privateKey string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("the endpoint is not a valid URL")
	}
	if !strings.HasPrefix(subscriber, "https:") {
		subscriber = "mailto:" + subscriber
	}
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{u.Scheme + "://" + u.Host, now.Add(vapidTokenLifetime).Unix(), subscriber})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(claims)

	key, err := vapidPrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256: the signature is R || S, each left-padded to 32 octets.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	pub, err := decodeVAPIDKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("VAPID public key: %w", err)
	}
	return "vapid t=" + signingInput + "." + base64.RawURLEncoding.EncodeToString(sig) +
		", k=" + base64.RawURLEncoding.EncodeToString(pub), nil
}

// vapidPrivateKey decodes FLEET_VAPID_PRIVATE_KEY: a base64url P-256 scalar,
// left-padded to 32 octets if a generator trimmed a leading zero.
func vapidPrivateKey(encoded string) (*ecdsa.PrivateKey, error) {
	raw, err := decodeVAPIDKey(encoded)
	if err != nil {
		return nil, fmt.Errorf("VAPID private key: %w", err)
	}
	if len(raw) > 32 {
		return nil, errors.New("VAPID private key: longer than a P-256 scalar")
	}
	scalar := make([]byte, 32)
	copy(scalar[32-len(raw):], raw)
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
	if err != nil {
		return nil, errors.New("VAPID private key: not a valid P-256 scalar")
	}
	return key, nil
}

// decodeVAPIDKey reads a VAPID key as padded, then unpadded, base64url.
func decodeVAPIDKey(key string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(key); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(key)
}

// decodeSubscriptionKey reads a subscription key as standard, then URL,
// base64, padding it first if it was stored without.
func decodeSubscriptionKey(key string) ([]byte, error) {
	if rem := len(key) % 4; rem != 0 {
		key += strings.Repeat("=", 4-rem)
	}
	if b, err := base64.StdEncoding.DecodeString(key); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(key)
}
