package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// A number is a credential as easily as a string: under a secret-named key
// it is redacted, whatever its kind, unless the key is count-like by the
// explicit suffix allowlist ("prompt_tokens" yes, "token" and "api_token"
// no). A boolean, one bit, is kept. The same rule holds inside a composite
// value (#2074 review).
func TestNumbersUnderSecretKeys(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x",
		"password", 12345678,
		"pin_secret", uint64(4242),
		"token", 99887766,
		"api_token", 3.5,
		"token_ttl", 30*time.Second,
		"prompt_tokens", 1200,
		"max_tokens", 4096,
		"retry_count", 3,
		"latency_ms", 87,
		"has_api_key", true,
		"cred", map[string]any{"password": 31415926, "prompt_tokens": 7},
	)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"password", "pin_secret", "token", "api_token"} {
		if m[k] != redactedValue {
			t.Errorf("%s = %v, want redacted", k, m[k])
		}
	}
	for k, want := range map[string]any{"prompt_tokens": float64(1200), "max_tokens": float64(4096), "retry_count": float64(3), "latency_ms": float64(87), "has_api_key": true, "token_ttl": float64(30 * time.Second)} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v kept", k, m[k], want)
		}
	}
	cred, _ := m["cred"].(map[string]any)
	if cred["password"] != redactedValue || cred["prompt_tokens"] != float64(7) {
		t.Errorf("composite = %v", cred)
	}
}

func TestCountLikeKeyIsASuffixAllowlist(t *testing.T) {
	for key, want := range map[string]bool{
		"prompt_tokens": true, "completion_tokens": true, "tokens": true, "token_count": true,
		"body_bytes": true, "elapsed_ms": true, "Content-Length": true,
		"token": false, "api_token": false, "password": false, "secret": false, "tokenizer": false,
	} {
		if got := countLikeKey(key); got != want {
			t.Errorf("countLikeKey(%q) = %v, want %v", key, got, want)
		}
	}
}
