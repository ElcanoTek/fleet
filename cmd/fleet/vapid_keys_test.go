package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestRunGenerateVAPIDKeys locks the output contract: exactly the three env
// lines (plus paste-safe # comments), base64url-decodable keys, and a fresh
// pair on every invocation. The public key must be a 65-byte uncompressed
// P-256 point (what pushManager.subscribe expects as applicationServerKey).
func TestRunGenerateVAPIDKeys(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runGenerateVAPIDKeys(&out, &errOut); code != 0 {
		t.Fatalf("exit code %d, want 0\noutput: %s", code, out.String())
	}

	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("non-env output line: %q", line)
		}
		vars[k] = v
	}
	if len(vars) != 3 {
		t.Fatalf("got %d env lines, want 3: %v", len(vars), vars)
	}

	pub, err := base64.RawURLEncoding.DecodeString(vars["FLEET_VAPID_PUBLIC_KEY"])
	if err != nil {
		t.Fatalf("public key is not base64url: %v", err)
	}
	if len(pub) != 65 || pub[0] != 0x04 {
		t.Errorf("public key is not an uncompressed P-256 point: len=%d first=%#x", len(pub), pub[0])
	}
	if _, err := base64.RawURLEncoding.DecodeString(vars["FLEET_VAPID_PRIVATE_KEY"]); err != nil {
		t.Errorf("private key is not base64url: %v", err)
	}
	if !strings.HasPrefix(vars["FLEET_VAPID_CONTACT"], "mailto:") {
		t.Errorf("contact %q is not a mailto: placeholder", vars["FLEET_VAPID_CONTACT"])
	}

	// A second run yields a different pair — never a fixed key.
	var out2, errOut2 bytes.Buffer
	if code := runGenerateVAPIDKeys(&out2, &errOut2); code != 0 {
		t.Fatalf("second run exit code %d", code)
	}
	if out.String() == out2.String() {
		t.Error("two runs produced identical keys")
	}
}

// TestGenerateVAPIDKeysErrorGoesToStderr pins the split that makes the
// documented `fleet generate-vapid-keys >> fleet.env` safe: on a generation
// failure stdout must stay EMPTY, because anything written there lands in the
// operator's env file. The message used to go to stdout, so a failed run
// appended "generate VAPID keys: ..." to the env file as a garbage line.
func TestGenerateVAPIDKeysErrorGoesToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	code := generateVAPIDKeysWith(func() (string, string, error) {
		return "", "", errors.New("no entropy")
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty on failure, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "no entropy") {
		t.Errorf("stderr = %q, want the generation error", errOut.String())
	}
}
