package webpush

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
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// RFC 8291 §5 and Appendix A: the worked example's inputs (sender and
// receiver keys, salt, auth secret, record size 4096) must encrypt "When I
// grow up, I want to be a watermelon" to exactly the example's body. The
// example adds no padding beyond the delimiter, so fill is off here; the
// production path differs only in the zeros it appends.
func TestEncrypt_RFC8291AppendixA(t *testing.T) {
	plaintext := b64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	if string(plaintext) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("plaintext = %q", plaintext)
	}
	asPrivate, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	asPublic := b64(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIg Dll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	if !bytes.Equal(asPrivate.PublicKey().Bytes(), asPublic) {
		t.Fatal("as_private does not give the example's as_public")
	}
	uaPublic := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcx aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	auth := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")

	got, err := encrypt(plaintext, auth, uaPublic, asPrivate, salt, false)
	if err != nil {
		t.Fatal(err)
	}
	want := b64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml"+
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT"+
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	if !bytes.Equal(got, want) {
		t.Fatalf("body =\n%s\nwant\n%s", base64.RawURLEncoding.EncodeToString(got), base64.RawURLEncoding.EncodeToString(want))
	}

	// The receiver's side of the same example decrypts it back.
	uaPrivate, err := ecdh.P256().NewPrivateKey(b64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	if pt := decryptAsBrowser(t, got, uaPrivate, auth); string(pt) != string(plaintext)+"\x02" {
		t.Fatalf("decrypted %q", pt)
	}
}

// decryptAsBrowser is the user agent's half of RFC 8291, written out
// independently of encrypt: it returns the record's plaintext, delimiter and
// padding included.
func decryptAsBrowser(t *testing.T, body []byte, uaPrivate *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body too short: %d", len(body))
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != recordSize {
		t.Fatalf("rs = %d", rs)
	}
	asPublicBytes := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	asPublic, err := ecdh.P256().NewPublicKey(asPublicBytes)
	if err != nil {
		t.Fatalf("keyid is not a P-256 point: %v", err)
	}
	secret, err := uaPrivate.ECDH(asPublic)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), uaPrivate.PublicKey().Bytes()...), asPublicBytes...)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return pt
}

type captureClient struct{ req *http.Request }

func (c *captureClient) Do(req *http.Request) (*http.Response, error) {
	c.req = req
	return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
}

// A real send, read back the way a browser and a push service would: the body
// is one full 4096-octet record that the subscription's private key decrypts
// to the payload, the delimiter and zeros; the headers are RFC 8030's; and the
// VAPID token verifies against the k= key with the claims a push service
// checks.
func TestSend_IsReadableByTheBrowserAndThePushService(t *testing.T) {
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	uaPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	client := &captureClient{}
	payload := []byte(`{"title":"✓ Task complete: x (1s)"}`)
	before := time.Now()
	resp, err := send(context.Background(), client, pushMessage{
		Endpoint:        "https://fcm.googleapis.com/fcm/send/abc:def",
		Auth:            base64.RawURLEncoding.EncodeToString(auth),
		P256dh:          base64.StdEncoding.EncodeToString(uaPrivate.PublicKey().Bytes()), // padded std, as some rows hold
		Payload:         payload,
		TTL:             3600,
		Urgency:         UrgencyHigh,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		Subscriber:      "ops@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	req := client.req

	for k, want := range map[string]string{
		"Content-Encoding": "aes128gcm", "Content-Type": "application/octet-stream",
		"Ttl": "3600", "Urgency": "high",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	body, _ := io.ReadAll(req.Body)
	if len(body) != recordSize {
		t.Fatalf("body is %d octets, want one full %d-octet record", len(body), recordSize)
	}
	pt := decryptAsBrowser(t, body, uaPrivate, auth)
	if !bytes.HasPrefix(pt, append(append([]byte{}, payload...), paddingDelimiter)) ||
		len(bytes.Trim(pt[len(payload)+1:], "\x00")) != 0 {
		t.Fatalf("plaintext is not payload || 0x02 || zeros: %q…", pt[:len(payload)+4])
	}

	// Authorization: vapid t=<jwt>, k=<public key>
	tok, k, ok := strings.Cut(strings.TrimPrefix(req.Header.Get("Authorization"), "vapid t="), ", k=")
	if !ok || !strings.HasPrefix(req.Header.Get("Authorization"), "vapid t=") {
		t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if k != pub {
		t.Errorf("k = %q, want the configured public key", k)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	if got := string(b64(t, parts[0])); got != `{"alg":"ES256","typ":"JWT"}` {
		t.Errorf("JWT header = %s", got)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != "mailto:ops@example.com" {
		t.Errorf("claims = %+v", claims)
	}
	if exp := time.Unix(claims.Exp, 0); exp.Before(before.Add(vapidTokenLifetime-time.Second)) || exp.After(time.Now().Add(vapidTokenLifetime)) {
		t.Errorf("exp = %s, want twelve hours ahead", exp)
	}
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, k))
	if err != nil {
		t.Fatal(err)
	}
	sig := b64(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the VAPID signature does not verify against k")
	}
}

// Keys keep the format webpush-go generated and read: an unpadded base64url
// 32-octet scalar and 65-octet uncompressed point; padded base64url is read
// too, and a scalar a generator trimmed of its leading zero is left-padded.
func TestVAPIDKeys_FormatAndCompatibility(t *testing.T) {
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	rawPriv, rawPub := b64(t, priv), b64(t, pub)
	if len(rawPriv) != 32 || len(rawPub) != 65 || rawPub[0] != 0x04 {
		t.Fatalf("key lengths %d/%d", len(rawPriv), len(rawPub))
	}
	key, err := vapidPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := key.PublicKey.Bytes(); !bytes.Equal(got, rawPub) {
		t.Fatal("the generated public key is not the private key's")
	}
	padded := base64.URLEncoding.EncodeToString(rawPriv)
	if k2, err := vapidPrivateKey(padded); err != nil || !k2.Equal(key) {
		t.Fatalf("padded base64url private key: %v", err)
	}

	// The RFC 8291 example's sender pair, as a VAPID pair.
	rfc, err := vapidPrivateKey("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rfc.PublicKey.Bytes(); base64.RawURLEncoding.EncodeToString(got) != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatal("RFC key pair does not round-trip")
	}

	// A 31-octet scalar (leading zero dropped) reads as the 32-octet one.
	short := make([]byte, 32)
	short[31] = 7
	shortKey, err := vapidPrivateKey(base64.RawURLEncoding.EncodeToString(short[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := ecdsa.ParseRawPrivateKey(elliptic.P256(), short); !shortKey.Equal(want) {
		t.Fatal("short scalar not left-padded")
	}
	for _, bad := range []string{"", "!!!", base64.RawURLEncoding.EncodeToString(make([]byte, 33)), base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := vapidPrivateKey(bad); err == nil {
			t.Errorf("vapidPrivateKey(%q) accepted", bad)
		} else if strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("error quotes the key: %v", err)
		}
	}
}

func TestDecodeSubscriptionKey(t *testing.T) {
	raw := []byte{0xfb, 0xff, 0xfe, 0x01, 0x02}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := decodeSubscriptionKey(enc.EncodeToString(raw))
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%v: got %x, %v", enc, got, err)
		}
	}
}

func TestEncrypt_RefusesWhatCannotBeSent(t *testing.T) {
	as, _ := ecdh.P256().GenerateKey(rand.Reader)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	salt := make([]byte, 16)
	auth := make([]byte, 16)
	// header 86 + tag 16 leaves 3994 octets: payload + delimiter.
	if _, err := encrypt(make([]byte, 3993), auth, ua.PublicKey().Bytes(), as, salt, true); err != nil {
		t.Fatalf("largest payload: %v", err)
	}
	if _, err := encrypt(make([]byte, 3994), auth, ua.PublicKey().Bytes(), as, salt, true); !errors.Is(err, errPayloadTooLarge) {
		t.Fatalf("oversized payload: err = %v", err)
	}
	if _, err := encrypt(nil, auth, []byte{0x04, 1, 2}, as, salt, true); err == nil {
		t.Fatal("an invalid p256dh point was accepted")
	}
}
