package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

func testSkills() []clientconfig.Skill {
	return []clientconfig.Skill{
		{Dir: "deploy", Name: "deploy", Description: "roll a release out", Path: "skills/deploy/SKILL.md"},
		{Dir: "research-report", Name: "research-report", Description: "write a cited research report", Path: "skills/research-report/SKILL.md"},
	}
}

// writeTestSkill lays down a minimal well-formed skill folder under dir so a
// Bundle{SkillsDir: dir} serves it through the same ReadSkills path production
// uses.
func writeTestSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatchSkillInvocation_Match(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string // matched skill name; "" = no match
	}{
		{"bare invocation", "/deploy", "deploy"},
		{"trailing args", "/deploy the staging box please", "deploy"},
		{"multiline body", "/deploy\nthen tell me what changed", "deploy"},
		{"hyphenated name", "/research-report on quantum error correction", "research-report"},
		{"windows newline", "/deploy\r\nsecond line", "deploy"},

		{"path does not match", "/etc/hosts has a weird entry", ""},
		{"mid-message slash", "please run /deploy for me", ""},
		{"case sensitive", "/Deploy", ""},
		{"prefix is not a match", "/deploy-now", ""},
		{"unknown token", "/unknown do things", ""},
		{"bare slash", "/", ""},
		{"slash then space", "/ deploy", ""},
		{"empty message", "", ""},
		{"leading whitespace", " /deploy", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block, matched := matchSkillInvocation(tc.message, testSkills())
			if matched != tc.want {
				t.Fatalf("matched = %q, want %q", matched, tc.want)
			}
			if tc.want == "" {
				if block != "" {
					t.Errorf("no match must append nothing, got %q", block)
				}
				return
			}
			if !strings.Contains(block, "[Skill invoked: "+tc.want+"]") {
				t.Errorf("block missing invocation marker: %q", block)
			}
			if !strings.Contains(block, "skills/"+tc.want+"/SKILL.md") {
				t.Errorf("block missing the SKILL.md path the agent must read: %q", block)
			}
		})
	}
}

func TestMatchSkillInvocation_NoSkills(t *testing.T) {
	if block, matched := matchSkillInvocation("/deploy", nil); block != "" || matched != "" {
		t.Errorf("empty roster must never match, got (%q, %q)", block, matched)
	}
}

func TestApplySkillInvocation(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "deploy", "roll a release out")
	s := &Server{clientConfig: &clientconfig.Bundle{SkillsDir: dir}}

	// The block is appended to the (already server-augmented) user message, but
	// matching runs against the RAW message the user typed.
	got := s.applySkillInvocation("augmented message", "/deploy staging")
	if !strings.HasPrefix(got, "augmented message") {
		t.Fatalf("original message dropped: %q", got)
	}
	if !strings.Contains(got, "[Skill invoked: deploy]") {
		t.Errorf("invocation block missing: %q", got)
	}

	// Unknown token → the message passes through untouched.
	if got := s.applySkillInvocation("msg", "/etc/hosts looks wrong"); got != "msg" {
		t.Errorf("unknown token must be a no-op, got %q", got)
	}

	// No bundle wired (mock/test boots) → nil-safe no-op.
	bare := &Server{}
	if got := bare.applySkillInvocation("msg", "/deploy"); got != "msg" {
		t.Errorf("nil clientConfig must be a no-op, got %q", got)
	}
}

func TestListSkills(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "deploy", "roll a release out")
	writeTestSkill(t, dir, "audit", "review the books")
	s := &Server{clientConfig: &clientconfig.Bundle{SkillsDir: dir}}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/skills", nil)
	w := httptest.NewRecorder()
	s.listSkills(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d want 200", w.Code)
	}
	var resp skillsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Skills) != 2 {
		t.Fatalf("got %d skills, want 2: %+v", len(resp.Skills), resp.Skills)
	}
	// ReadSkills sorts by name.
	if resp.Skills[0].Name != "audit" || resp.Skills[1].Name != "deploy" {
		t.Errorf("unexpected roster order: %+v", resp.Skills)
	}
	if resp.Skills[1].Description != "roll a release out" {
		t.Errorf("description not surfaced: %+v", resp.Skills[1])
	}
}

func TestListSkills_NilBundleIsEmptyArray(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/skills", nil)
	w := httptest.NewRecorder()
	s.listSkills(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d want 200", w.Code)
	}
	// The empty roster must encode as [], not null — the web client indexes it.
	if body := strings.TrimSpace(w.Body.String()); !strings.Contains(body, `"skills":[]`) {
		t.Errorf("empty roster must encode as an empty array, got %q", body)
	}
}

func TestListSkills_MethodNotAllowed(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/skills", nil)
	w := httptest.NewRecorder()
	s.listSkills(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d want 405", w.Code)
	}
}
