package clientconfig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPersonasManifestParse verifies the personas: block parses into PersonaDef
// entries and the PersonaToolPolicy accessor resolves them by basename (#294).
func TestPersonasManifestParse(t *testing.T) {
	dir := t.TempDir()
	manifest := `
personas:
  - name: code-reviewer
    tool_permissions:
      allow:
        - bash
        - run_python
        - mcp:filesystem/*
      deny:
        - mcp:email/*
        - send_email
  - name: executive-assistant
    tool_permissions:
      deny:
        - bash
        - run_python
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Personas) != 2 {
		t.Fatalf("Personas = %d, want 2", len(b.Personas))
	}

	cr, ok := b.PersonaToolPolicy("code-reviewer")
	if !ok {
		t.Fatal("PersonaToolPolicy(code-reviewer) not found")
	}
	if !slices.Equal(cr.Allow, []string{"bash", "run_python", "mcp:filesystem/*"}) {
		t.Errorf("code-reviewer allow = %v", cr.Allow)
	}
	if !slices.Equal(cr.Deny, []string{"mcp:email/*", "send_email"}) {
		t.Errorf("code-reviewer deny = %v", cr.Deny)
	}

	// The accessor normalizes a .yaml-suffixed / path-prefixed reference.
	if _, ok := b.PersonaToolPolicy("personas/code-reviewer.yaml"); !ok {
		t.Error("PersonaToolPolicy should resolve a personas/<name>.yaml reference")
	}

	ea, ok := b.PersonaToolPolicy("executive-assistant")
	if !ok {
		t.Fatal("PersonaToolPolicy(executive-assistant) not found")
	}
	if len(ea.Allow) != 0 || !slices.Equal(ea.Deny, []string{"bash", "run_python"}) {
		t.Errorf("executive-assistant policy = %+v", ea)
	}
}

// TestPersonasAbsentSectionIsBackwardCompatible: a bundle with no personas:
// block loads fine and PersonaToolPolicy reports "not found" for any name, so
// the drivers fall back to the permissive default (all tools).
func TestPersonasAbsentSectionIsBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Personas) != 0 {
		t.Fatalf("Personas = %d, want 0", len(b.Personas))
	}
	if _, ok := b.PersonaToolPolicy("anything"); ok {
		t.Error("PersonaToolPolicy on an empty bundle should report not-found")
	}
}

// TestPersonaToolPolicyDefensiveCopy ensures a caller mutating the returned
// slices cannot corrupt the bundle's stored policy.
func TestPersonaToolPolicyDefensiveCopy(t *testing.T) {
	dir := t.TempDir()
	manifest := `
personas:
  - name: p
    tool_permissions:
      allow: [bash]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, _ := b.PersonaToolPolicy("p")
	if len(got.Allow) > 0 {
		got.Allow[0] = "mutated"
	}
	again, _ := b.PersonaToolPolicy("p")
	if again.Allow[0] != "bash" {
		t.Fatalf("PersonaToolPolicy must return a defensive copy; bundle was mutated to %v", again.Allow)
	}
}

func TestPersonasValidationRejectsBlankName(t *testing.T) {
	dir := t.TempDir()
	manifest := `
personas:
  - name: ""
    tool_permissions:
      allow: [bash]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected load to fail on a blank persona name")
	}
}

func TestPersonasValidationRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	manifest := `
personas:
  - name: dup
    tool_permissions:
      allow: [bash]
  - name: dup
    tool_permissions:
      deny: [bash]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected load to fail on a duplicate persona name")
	}
}
