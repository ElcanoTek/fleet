// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package websession

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// These cases are ported from web/src/app/lib/auth.test.ts, the TypeScript
// verifier this package replaced (ADR-0091). The cookie format must not move:
// sessions minted by the Next.js tier before an upgrade stay valid after it.

const secret = "test-session-secret-0123456789abcdef"

const hour = 60 * 60

func clockAt(sec int64) func() time.Time {
	return func() time.Time { return time.Unix(sec, 0) }
}

// signRaw signs an arbitrary payload with the secret, standing in for a cookie
// minted by a build with different claims (or by the TypeScript tier).
func signRaw(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func decode(t *testing.T, tok string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(tok, ".", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mintElcano(t *testing.T, priv ed25519.PrivateKey, claims map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}

func TestVerifiesATokenMintedByTheTypeScriptTier(t *testing.T) {
	// Byte-for-byte the shape auth.ts#createSessionToken produced:
	// JSON.stringify({email, exp, idle, epoch}) → base64url → HMAC over it.
	future := time.Now().Add(time.Hour).Unix()
	tok := signRaw(t, map[string]any{"email": "dave@x.com", "exp": future, "idle": future, "epoch": "e1"})
	s := NewHMAC(secret).Verify(tok)
	if s == nil || s.Email != "dave@x.com" || s.Epoch != "e1" || s.Source != SourcePassword {
		t.Fatalf("got %+v", s)
	}
}

func TestEpochClaim(t *testing.T) {
	h := NewHMAC(secret)
	future := time.Now().Add(time.Hour).Unix()

	t.Run("round-trips the epoch a token was minted with", func(t *testing.T) {
		tok, err := h.Mint("Dave@X.com", "abcdef0123456789")
		if err != nil {
			t.Fatal(err)
		}
		s := h.Verify(tok)
		if s == nil || s.Email != "dave@x.com" || s.Epoch != "abcdef0123456789" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("keeps central OIDC identity distinct from password sessions", func(t *testing.T) {
		tok, _ := h.MintOIDC("dave@x.com", "external-epoch", "https://auth.example.com", "account-123")
		s := h.Verify(tok)
		if s == nil || s.Source != SourceOIDC || s.Issuer != "https://auth.example.com" || s.Subject != "account-123" || s.Epoch != "external-epoch" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("refuses a correctly-signed token without an epoch", func(t *testing.T) {
		if h.Verify(signRaw(t, map[string]any{"email": "dave@x.com", "exp": future, "idle": future})) != nil {
			t.Fatal("claimless cookie admitted")
		}
	})
	t.Run("refuses an empty epoch", func(t *testing.T) {
		if h.Verify(signRaw(t, map[string]any{"email": "dave@x.com", "exp": future, "idle": future, "epoch": ""})) != nil {
			t.Fatal("empty epoch admitted")
		}
	})
	t.Run("refuses an oidc source without issuer/subject", func(t *testing.T) {
		if h.Verify(signRaw(t, map[string]any{"email": "d@x.com", "exp": future, "idle": future, "epoch": "e", "source": "oidc"})) != nil {
			t.Fatal("oidc without issuer admitted")
		}
	})
	t.Run("refuses an unknown source", func(t *testing.T) {
		if h.Verify(signRaw(t, map[string]any{"email": "d@x.com", "exp": future, "idle": future, "epoch": "e", "source": "elcano"})) != nil {
			t.Fatal("forged elcano source admitted")
		}
	})
}

func TestHMACRejects(t *testing.T) {
	h := NewHMAC(secret)
	tok, _ := h.Mint("a@x.com", "e")
	for name, bad := range map[string]string{
		"empty":           "",
		"no dot":          "abc",
		"bad signature":   tok[:strings.IndexByte(tok, '.')] + ".AAAA",
		"other secret":    func() string { t, _ := NewHMAC("another").Mint("a@x.com", "e"); return t }(),
		"tampered body":   base64.RawURLEncoding.EncodeToString([]byte(`{"email":"b@x.com"}`)) + tok[strings.IndexByte(tok, '.'):],
		"empty signature": tok[:strings.IndexByte(tok, '.')+1],
	} {
		if h.Verify(bad) != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if NewHMAC("").Verify(tok) != nil {
		t.Error("an unconfigured secret must refuse every token")
	}
	if _, err := NewHMAC("").Mint("a@x.com", "e"); err == nil {
		t.Error("an unconfigured secret must refuse to mint")
	}
}

func TestLifetimes(t *testing.T) {
	const t0 = 1_800_000_000

	t.Run("mints exp one day out and idle twelve hours out", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).Mint("bob@x.com", "epoch-1")
		p := decode(t, tok)
		if p["exp"] != float64(t0+24*hour) || p["idle"] != float64(t0+12*hour) {
			t.Fatalf("got %v", p)
		}
		if AbsoluteSeconds != 24*hour || IdleSeconds != 12*hour {
			t.Fatal("lifetime constants moved")
		}
	})
	t.Run("refuses a session idle for twelve hours", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).Mint("bob@x.com", "epoch-1")
		if NewHMAC(secret).WithClock(clockAt(t0+12*hour-1)).Verify(tok) == nil {
			t.Fatal("refused before the idle deadline")
		}
		if NewHMAC(secret).WithClock(clockAt(t0+12*hour+1)).Verify(tok) != nil {
			t.Fatal("admitted after the idle deadline")
		}
	})
	t.Run("refuses a signed token with no idle claim", func(t *testing.T) {
		future := time.Now().Add(time.Hour).Unix()
		if NewHMAC(secret).Verify(signRaw(t, map[string]any{"email": "bob@x.com", "exp": future, "epoch": "e"})) != nil {
			t.Fatal("pre-ADR cookie grandfathered")
		}
	})
	t.Run("does not re-mint under a minute", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).Mint("bob@x.com", "epoch-1")
		h := NewHMAC(secret).WithClock(clockAt(t0 + 30))
		if _, _, ok := h.Refresh(h.Verify(tok)); ok {
			t.Fatal("re-minted within the touch window")
		}
	})
	t.Run("re-mints after a minute copying exp and every identity claim", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).MintOIDC("Bob@x.com", "epoch-9", "https://auth.example.com", "sub-1")
		h := NewHMAC(secret).WithClock(clockAt(t0 + 61))
		refreshed, maxAge, ok := h.Refresh(h.Verify(tok))
		if !ok {
			t.Fatal("did not re-mint")
		}
		p := decode(t, refreshed)
		want := map[string]any{"email": "bob@x.com", "exp": float64(t0 + 24*hour), "idle": float64(t0 + 61 + 12*hour), "epoch": "epoch-9", "source": "oidc", "issuer": "https://auth.example.com", "subject": "sub-1"}
		for k, v := range want {
			if p[k] != v {
				t.Errorf("%s = %v, want %v", k, p[k], v)
			}
		}
		if maxAge != 24*hour-61 {
			t.Errorf("maxAge = %d", maxAge)
		}
		if s := h.Verify(refreshed); s == nil || s.Subject != "sub-1" {
			t.Fatal("re-minted token does not verify")
		}
	})
	t.Run("activity never extends the absolute deadline", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).Mint("bob@x.com", "epoch-1")
		for _, step := range []struct{ at, wantIdle int64 }{{11 * hour, t0 + 23*hour}, {22 * hour, t0 + 24*hour}} {
			h := NewHMAC(secret).WithClock(clockAt(t0 + step.at))
			next, _, ok := h.Refresh(h.Verify(tok))
			if !ok {
				t.Fatalf("no refresh at +%d", step.at)
			}
			p := decode(t, next)
			if p["exp"] != float64(t0+24*hour) || p["idle"] != float64(step.wantIdle) {
				t.Fatalf("got %v", p)
			}
			tok = next
		}
		if NewHMAC(secret).WithClock(clockAt(t0+24*hour-1)).Verify(tok) == nil {
			t.Fatal("refused before exp")
		}
		if NewHMAC(secret).WithClock(clockAt(t0+24*hour+1)).Verify(tok) != nil {
			t.Fatal("admitted after exp")
		}
	})
	t.Run("stops re-minting once idle sits on the absolute deadline", func(t *testing.T) {
		tok, _ := NewHMAC(secret).WithClock(clockAt(t0)).Mint("bob@x.com", "epoch-1")
		h1 := NewHMAC(secret).WithClock(clockAt(t0 + 11*hour))
		t1, _, _ := h1.Refresh(h1.Verify(tok))
		h2 := NewHMAC(secret).WithClock(clockAt(t0 + 22*hour + 30*60))
		t2, _, ok := h2.Refresh(h2.Verify(t1))
		if !ok || decode(t, t2)["idle"] != float64(t0+24*hour) {
			t.Fatal("second refresh did not cap at exp")
		}
		h3 := NewHMAC(secret).WithClock(clockAt(t0 + 22*hour + 35*60))
		s3 := h3.Verify(t2)
		if s3 == nil {
			t.Fatal("refused a live session")
		}
		if _, _, ok := h3.Refresh(s3); ok {
			t.Fatal("re-minted with nothing to move")
		}
	})
}

func TestElcano(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	future := time.Now().Add(time.Hour).Unix()

	t.Run("accepts a valid token and lowercases the email", func(t *testing.T) {
		c := VerifyElcanoToken(pub, mintElcano(t, priv, map[string]any{"email": "Carol@Elcanotek.com", "tenant": "elcanotek.com", "exp": future}))
		if c == nil || c.Email != "carol@elcanotek.com" || c.Tenant != "elcanotek.com" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("rejects tampering, expiry, a missing email and another key", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]any{"email": "c@x.com", "exp": future})
		tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"evil@x.com","exp":9999999999}`)) + tok[strings.IndexByte(tok, '.'):]
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		cases := map[string]struct {
			pub ed25519.PublicKey
			tok string
		}{
			"tampered":  {pub, tampered},
			"expired":   {pub, mintElcano(t, priv, map[string]any{"email": "c@x.com", "exp": time.Now().Add(-time.Minute).Unix()})},
			"no email":  {pub, mintElcano(t, priv, map[string]any{"exp": future})},
			"other key": {otherPub, tok},
			"malformed": {pub, "not-a-token"},
			"dot first": {pub, "." + tok},
			"short key": {ed25519.PublicKey("short"), tok},
			"nil key":   {nil, tok},
		}
		for name, c := range cases {
			if VerifyElcanoToken(c.pub, c.tok) != nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
	t.Run("ParseElcanoPubKey", func(t *testing.T) {
		if ParseElcanoPubKey(base64.StdEncoding.EncodeToString(pub)) == nil {
			t.Error("valid key refused")
		}
		for _, bad := range []string{"", "  ", "not base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
			if ParseElcanoPubKey(bad) != nil {
				t.Errorf("%q parsed", bad)
			}
		}
	})
}

func TestResolverPrecedence(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	future := time.Now().Add(time.Hour).Unix()
	r := &Resolver{HMAC: NewHMAC(secret), ElcanoPubKey: pub}
	hmacTok, _ := r.HMAC.Mint("alice@x.com", "e")
	elcanoTok := mintElcano(t, priv, map[string]any{"email": "carol@x.com", "exp": future})

	if s := r.Resolve(hmacTok, ""); s == nil || s.Source != SourcePassword {
		t.Fatalf("password: %+v", s)
	}
	if s := r.Resolve("", elcanoTok); s == nil || s.Source != SourceElcano || s.Epoch != "" {
		t.Fatalf("elcano: %+v", s)
	}
	if s := r.Resolve(hmacTok, elcanoTok); s == nil || s.Email != "alice@x.com" {
		t.Fatalf("HMAC must win when both are present: %+v", s)
	}
	if s := r.Resolve("garbage", elcanoTok); s == nil || s.Email != "carol@x.com" {
		t.Fatalf("an invalid HMAC cookie falls back to elcano: %+v", s)
	}
	if r.Resolve("garbage", "garbage") != nil {
		t.Fatal("neither valid must be nil")
	}
	if _, _, ok := r.HMAC.Refresh(r.Resolve("", elcanoTok)); ok {
		t.Fatal("fleet cannot re-mint the auth service's cookie")
	}
}

// A deployment that signs people in through central Auth only refuses the
// cookie a password login minted, whether it names its source or (as Mint
// writes it) leaves the source out, and falls back to the elcano_auth cookie
// rather than stranding a browser that holds both. Central sessions pass.
func TestResolverRefusesPasswordSessions(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	future := time.Now().Add(time.Hour).Unix()
	r := &Resolver{HMAC: NewHMAC(secret), ElcanoPubKey: pub, RefusePassword: true}
	sourceless, _ := r.HMAC.Mint("alice@x.com", "e")
	now := time.Now().Unix()
	exp, idle := freshDeadlines(now)
	named, err := r.HMAC.encode(payload{Email: "alice@x.com", Exp: exp, Idle: idle, Epoch: "e", Source: string(SourcePassword)})
	if err != nil {
		t.Fatal(err)
	}
	oidcTok, _ := r.HMAC.MintOIDC("dave@x.com", "e", "https://auth.example.com", "sub-1")
	elcanoTok := mintElcano(t, priv, map[string]any{"email": "carol@x.com", "exp": future})

	for name, tok := range map[string]string{"source-less": sourceless, "source password": named} {
		if s := r.HMAC.Verify(tok); s == nil || s.Source != SourcePassword {
			t.Fatalf("%s: fixture is not a password session: %+v", name, s)
		}
		if s := r.Resolve(tok, ""); s != nil {
			t.Errorf("%s password cookie accepted: %+v", name, s)
		}
		if s := r.Resolve(tok, elcanoTok); s == nil || s.Source != SourceElcano || s.Email != "carol@x.com" {
			t.Errorf("%s password cookie hid the elcano cookie: %+v", name, s)
		}
	}
	if s := r.Resolve(oidcTok, ""); s == nil || s.Source != SourceOIDC || s.Email != "dave@x.com" {
		t.Errorf("central session refused: %+v", s)
	}
	if s := r.Resolve("", elcanoTok); s == nil || s.Source != SourceElcano {
		t.Errorf("elcano cookie refused: %+v", s)
	}
}
