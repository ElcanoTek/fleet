package clientconfig

import (
	"bytes"
	"log"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// manifestYAMLTags collects every yaml key reachable from the manifest type.
func manifestYAMLTags(t reflect.Type, seen map[reflect.Type]bool, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); tag != "" && tag != "-" {
			out[tag] = true
		}
		manifestYAMLTags(f.Type, seen, out)
	}
}

// TestEnvNameFieldsCoverTheSchema — every manifest field that names an env
// variable (*_env, *_vars, enabled_groups) is classified exactly once: read by
// fleet (envNameFields, warned about and admitted) or a connector env key
// (envKeyFields). A new such field fails here until it is classified, and
// every listed field still exists.
func TestEnvNameFieldsCoverTheSchema(t *testing.T) {
	tags := map[string]bool{}
	manifestYAMLTags(reflect.TypeFor[manifest](), map[reflect.Type]bool{}, tags)
	for tag := range tags {
		if !strings.HasSuffix(tag, "_env") && !strings.HasSuffix(tag, "_vars") && tag != "enabled_groups" {
			continue
		}
		_, key := envKeyFields[tag]
		if envNameFields[tag] == key {
			t.Errorf("manifest field %q must be in exactly one of envNameFields / envKeyFields (clientconfig.go)", tag)
		}
	}
	for tag := range envNameFields {
		if !tags[tag] {
			t.Errorf("envNameFields lists %q, which no manifest type has", tag)
		}
	}
	for tag := range envKeyFields {
		if !tags[tag] {
			t.Errorf("envKeyFields lists %q, which no manifest type has", tag)
		}
	}
}

// The warning list and the lookup list are the same: every envNameFields
// field's value reaches literalEnvVarNames (the names admitted and read), and
// a removed spelling in any of them warns — while the same string in an
// ordinary field (a description, an argument) does not.
func TestEnvNameFieldsDriveTheWarningAndTheLookup(t *testing.T) {
	unsetTracked(t, "N_API", "N_EN", "N_GRP", "N_ACCT", "N_IDENT", "N_HMAC", "N_TOK")
	armEnvFile(t, "")
	dir := writeManifest(t, `
mcp_servers:
  - name: c
    command: python3
    args: ["CHAT_SERVER_TOKEN"]
    description: "CHAT_SERVER_TOKEN"
    enabled_env: ["N_EN"]
    enabled_groups: [["N_GRP"]]
    account_vars: ["N_ACCT"]
    env:
      N_IDENT: "x"
    identity_env: ["N_IDENT"]
webhook_triggers:
  - slug: a
    hmac_secret_env: N_HMAC
    notify_user: u
  - slug: b
    token_secret_env: N_TOK
    notify_user: u
providers:
  - name: p
    type: openai
    api_key_env: N_API
`)
	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var literal []string
	b.literalEnvVarNames(func(n string) { literal = append(literal, n) })
	for _, want := range []string{"N_API", "N_EN", "N_GRP", "N_ACCT", "N_IDENT", "N_HMAC", "N_TOK"} {
		if !slices.Contains(literal, want) {
			t.Errorf("literalEnvVarNames lacks %s (an envNameFields field it must read)", want)
		}
	}
	if strings.Contains(buf.String(), "CHAT_SERVER_TOKEN") {
		t.Errorf("a description / argument equal to a removed spelling was warned about:\n%s", buf.String())
	}
	if ws, err := RetiredManifestRefWarnings(dir); err != nil || len(ws) != 0 {
		t.Errorf("RetiredManifestRefWarnings = %q, %v; want none", ws, err)
	}
}
