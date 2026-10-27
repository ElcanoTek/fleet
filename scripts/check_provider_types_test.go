// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The supported LLM provider types live in one Go table
// (agentcore.ProviderTypes / LookupProviderType), which the bundle loader, the
// admin store and the resolver all check. The admin form repeats the list in
// one web file for its labels, because the web bundle cannot import Go. This
// test keeps that copy honest: the same types in the same order, and the same
// required fields, so the form neither offers a type the server rejects nor
// lets through a row the server will refuse. windowNeeded is pinned too: the
// server requires a context window to enable every type except OpenRouter
// (httpapi validateLLMProviderRow), whose resolver ignores a declared one.
func TestProviderTypesAreInSync(t *testing.T) {
	root := repoRoot(t)
	web := readFile(t, root, "web/src/app/settings/admin/providers/page.tsx")

	start := strings.Index(web, "const PROVIDER_TYPES: Record<")
	if start < 0 {
		t.Fatal("providers/page.tsx: no `const PROVIDER_TYPES: Record<` map")
	}
	end := strings.Index(web[start:], "\n};\n")
	if end < 0 {
		t.Fatal("providers/page.tsx: PROVIDER_TYPES map has no closing `};`")
	}
	block := web[start : start+end]

	entryRE := regexp.MustCompile(`(?m)^  ([a-z]+): \{\n((?:    .*\n)+?)  \},?$`)
	entries := entryRE.FindAllStringSubmatch(block+"\n", -1)
	webTypes := make([]string, 0, len(entries))
	for _, e := range entries {
		webTypes = append(webTypes, e[1])
	}
	goTypes := make([]string, 0, len(agentcore.ProviderTypes()))
	for _, typ := range agentcore.ProviderTypes() {
		goTypes = append(goTypes, string(typ))
	}
	if strings.Join(webTypes, ",") != strings.Join(goTypes, ",") {
		t.Fatalf("PROVIDER_TYPES keys = %v, agentcore.ProviderTypes() = %v: add or remove the type in both, in the same order", webTypes, goTypes)
	}

	flag := func(body, name string) (bool, bool) {
		m := regexp.MustCompile(`(?m)^    ` + name + `: (true|false),$`).FindStringSubmatch(body)
		if m == nil {
			return false, false
		}
		return m[1] == "true", true
	}
	for _, e := range entries {
		spec, _ := agentcore.LookupProviderType(e[1])
		for _, f := range []struct {
			name string
			want bool
		}{
			{"keyNeeded", spec.APIKeyRequired},
			{"urlNeeded", spec.BaseURLRequired},
			{"windowNeeded", spec.Type != agentcore.ProviderTypeOpenRouter},
		} {
			got, ok := flag(e[2], f.name)
			if !ok {
				t.Errorf("PROVIDER_TYPES.%s: no `%s: true|false,` line", e[1], f.name)
				continue
			}
			if got != f.want {
				t.Errorf("PROVIDER_TYPES.%s.%s = %v, but the Go table says %v", e[1], f.name, got, f.want)
			}
		}
	}
}
