package envreg

import (
	"regexp"
	"testing"
)

// credentialName matches a variable NAME that says its value is a credential.
var credentialName = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|PW|HEADERS|DSN)($|_)|DATABASE_URL$`)

// notSecret are credential-sounding names whose values are deliberately not
// secrets, each with the reason.
var notSecret = map[string]string{
	"FLEET_VAPID_PUBLIC_KEY": "a PUBLIC key, served to browsers",
	"FLEET_SENTRY_DSN":       "a public ingest identifier; explain still strips its userinfo (the key part) like any URL",

	"FLEET_TLS_KEY_FILE":                  "a PATH to the key file, not the key",
	"FLEET_OTEL_CLIENT_KEY_FILE":          "a PATH to the trace-export mTLS key file, not the key",
	"FLEET_OIDC_ALLOW_PASSWORD":           "a boolean break-glass switch",
	"FLEET_SANDBOX_K8S_IMAGE_PULL_SECRET": "the NAME of a Kubernetes Secret object, not its content",
}

// TestCredentialValuesAreSecret — every setting whose name says it holds a
// credential, and every DSN, is marked Secret, so `fleet config explain`
// redacts it (Codex P1 on #2057: OPENAI_CUSTOM_HEADERS was printed raw). The
// values that can carry a credential without saying so by name are pinned
// explicitly.
func TestCredentialValuesAreSecret(t *testing.T) {
	for _, e := range All() {
		if e.Type == TypeDSN && !e.Secret {
			t.Errorf("%s is a DSN but not Secret", e.Name)
		}
		if !credentialName.MatchString(e.Name) || e.Secret {
			continue
		}
		if _, ok := notSecret[e.Name]; ok {
			continue
		}
		t.Errorf("%s looks like a credential but is not marked Secret (or add it to notSecret with the reason)", e.Name)
	}
	for _, name := range []string{"OPENAI_CUSTOM_HEADERS", "FLEET_WEBHOOK_URL", "FLEET_WEBHOOK_BODY_TEMPLATE", "DATABASE_URL", "FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL"} {
		if e, _ := Find(name); !e.Secret {
			t.Errorf("%s can carry a credential and must be Secret", name)
		}
	}
}
