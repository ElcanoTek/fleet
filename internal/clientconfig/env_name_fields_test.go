package clientconfig

import (
	"bytes"
	"log"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// manifestSchemaPaths collects the schema path ("mcp_servers[].enabled_env")
// of every typed field reachable from the manifest type. Opaque fields (any,
// map[string]any) end the walk: nothing under them is a typed field.
func manifestSchemaPaths(t reflect.Type, prefix string, depth int, out map[string]bool) {
	if depth > 12 {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		manifestSchemaPaths(t.Elem(), prefix+"[]", depth+1, out)
		return
	case reflect.Struct:
	default:
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" || !f.IsExported() {
			continue
		}
		p := tag
		if prefix != "" {
			p = prefix + "." + tag
		}
		out[p] = true
		manifestSchemaPaths(f.Type, p, depth+1, out)
	}
}

// TestEnvNameFieldsCoverTheSchema — every typed manifest field that names an
// env variable (*_env, *_vars, enabled_groups) is classified exactly once, by
// its schema path: read by fleet (envNameFields: checked by the scanner and
// admitted) or a connector env key (envKeyFields). A new such field fails
// here until it is classified, and every listed path still exists.
func TestEnvNameFieldsCoverTheSchema(t *testing.T) {
	paths := map[string]bool{}
	manifestSchemaPaths(reflect.TypeFor[manifest](), "", 0, paths)
	for p := range paths {
		leaf := p[strings.LastIndex(p, ".")+1:]
		if !strings.HasSuffix(leaf, "_env") && !strings.HasSuffix(leaf, "_vars") && leaf != "enabled_groups" {
			continue
		}
		_, key := envKeyFields[p]
		if envNameFields[p] == key {
			t.Errorf("manifest field %q must be in exactly one of envNameFields / envKeyFields (retired_refs.go)", p)
		}
	}
	for p := range envNameFields {
		if !paths[p] {
			t.Errorf("envNameFields lists %q, which is not a typed manifest field", p)
		}
	}
	for p := range envKeyFields {
		if !paths[p] {
			t.Errorf("envKeyFields lists %q, which is not a typed manifest field", p)
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
