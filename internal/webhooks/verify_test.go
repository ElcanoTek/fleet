package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

func githubSig(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMACSHA256(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "s3cr3t"
	good := githubSig(t, secret, body)

	cases := []struct {
		name   string
		body   []byte
		secret string
		header string
		want   bool
	}{
		{"valid github-style", body, secret, good, true},
		{"valid raw hex (no prefix)", body, secret, good[len("sha256="):], true},
		{"uppercase hex accepted", body, secret, "sha256=" + upper(good[len("sha256="):]), true},
		{"wrong secret", body, "other", good, false},
		{"tampered body", []byte(`{"hello":"evil"}`), secret, good, false},
		{"empty secret fails closed", body, "", good, false},
		{"empty header", body, secret, "", false},
		{"malformed (too short)", body, secret, "sha256=abcd", false},
		{"non-hex garbage of right length", body, secret, "sha256=" + repeat("z", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyHMACSHA256(tc.body, tc.secret, tc.header); got != tc.want {
				t.Errorf("VerifyHMACSHA256 = %v, want %v", got, tc.want)
			}
		})
	}
}

func slackSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySlackSignature(t *testing.T) {
	body := []byte(`token=x&team_id=T1&event=hi`)
	secret := "slack-signing-secret"
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	good := slackSig(secret, ts, body)

	cases := []struct {
		name string
		body []byte
		ts   string
		sig  string
		now  time.Time
		want bool
	}{
		{"valid", body, ts, good, now, true},
		{"within window (2m old)", body, ts, good, now.Add(2 * time.Minute), true},
		{"stale beyond window", body, ts, good, now.Add(10 * time.Minute), false},
		{"future beyond window", body, ts, good, now.Add(-10 * time.Minute), false},
		{"tampered body", []byte("evil"), ts, good, now, false},
		{"wrong signature", body, ts, "v0=deadbeef", now, false},
		{"unparseable timestamp", body, "not-a-number", good, now, false},
		{"empty timestamp", body, "", good, now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifySlackSignature(tc.body, secret, tc.ts, tc.sig, tc.now); got != tc.want {
				t.Errorf("VerifySlackSignature = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("empty secret fails closed", func(t *testing.T) {
		if VerifySlackSignature(body, "", ts, good, now) {
			t.Error("empty secret must fail closed")
		}
	})
}

func TestNewDummySecret(t *testing.T) {
	a, b := NewDummySecret(), NewDummySecret()
	if a == "" || b == "" {
		t.Fatal("dummy secret must be non-empty")
	}
	if a == b {
		t.Error("two dummy secrets should differ (crypto/rand)")
	}
	// A dummy secret must never validate an arbitrary attacker signature.
	body := []byte("probe")
	if VerifyHMACSHA256(body, a, githubSig(t, "attacker-guess", body)) {
		t.Error("dummy secret must not validate an attacker-chosen signature")
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 32
		}
	}
	return string(out)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
