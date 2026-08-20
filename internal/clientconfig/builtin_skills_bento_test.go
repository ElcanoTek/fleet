package clientconfig

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The bento-slides pack ships a VENDORED third-party asset — the Bento app
// shell, redistributed unmodified (see its templates/NOTICE.md). Nothing else
// in the pipeline watches it: govulncheck is Go-only, the Grype job scans the
// sandbox image rather than this repo, and Dependabot has no manifest to read.
// These tests are the only tripwire, so they pin the bytes and the two
// structural facts the helper script depends on.
const (
	bentoTemplateRel    = "bento-slides/templates/Bento_Slides.bento.html"
	bentoTemplateSize   = 689316
	bentoTemplateSHA256 = "9fef088beb763e86a7c13b6b5e2226816a9e8e1c61331f0c5270fdd5cf538424"

	bentoReferenceRel    = "bento-slides/references/authoring.md"
	bentoReferenceSHA256 = "82d8d7291a772dd3da4af112233a04504a8480a63ac68ab07bf7b8828e7add4f"

	bentoHelperRel = "bento-slides/scripts/bento_doc.py"

	// The id of the guard fleet adds to upstream's shell to make a deck
	// offline-only. Kept in lockstep with bento_doc.py.
	bentoGuardID = "fleet-offline-deck"

	// The document block's opening tag. bento_doc.py splices on this exact
	// string and requires it to be unique; a re-vendor that changed either
	// fact would break deck authoring at runtime, so assert it here.
	bentoDocAnchor = `<script type="application/bento+json" id="bento-doc">`
)

// materializeBentoPack returns the merged skills dir for a bundle with no
// skills of its own, so only the embedded pack is present.
func materializeBentoPack(t *testing.T) string {
	t.Helper()
	merged, err := materializeMergedSkills(filepath.Join(t.TempDir(), "no-bundle-skills"), true, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return merged
}

// The vendored shell must survive the embed + WalkDir materialization byte for
// byte. A truncated copy, a CRLF rewrite from a contributor with
// core.autocrlf=true (which .gitattributes guards against), or an undeclared
// version bump all fail here rather than shipping a deck nobody can open.
func TestBentoTemplateMaterializesIntact(t *testing.T) {
	merged := materializeBentoPack(t)

	got, err := os.ReadFile(filepath.Join(merged, bentoTemplateRel))
	if err != nil {
		t.Fatalf("read materialized template: %v", err)
	}
	if len(got) != bentoTemplateSize {
		t.Errorf("template is %d bytes, want %d", len(got), bentoTemplateSize)
	}
	if sum := hex.EncodeToString(sha256Sum(got)); sum != bentoTemplateSHA256 {
		t.Errorf("template sha256 = %s, want %s\n"+
			"If this is a deliberate Bento upgrade, update the pinned size/digest "+
			"here AND the provenance table in templates/NOTICE.md in the same commit.",
			sum, bentoTemplateSHA256)
	}

	// The materialized bytes must equal the embedded bytes: proves the
	// WalkDir copy transforms nothing on the way out of embed.FS.
	embedded, err := builtinSkillsFS.ReadFile(builtinSkillsRoot + "/" + bentoTemplateRel)
	if err != nil {
		t.Fatalf("read embedded template: %v", err)
	}
	if !bytes.Equal(got, embedded) {
		t.Error("materialized template differs from the embedded copy")
	}

	if n := bytes.Count(got, []byte(bentoDocAnchor)); n != 1 {
		t.Errorf("document block anchor appears %d times, want exactly 1 "+
			"(bento_doc.py splices on it being unique)", n)
	}

	// The shipped shell's block is empty; `new` seeds a document into it.
	start := bytes.Index(got, []byte(bentoDocAnchor)) + len(bentoDocAnchor)
	end := bytes.Index(got[start:], []byte("</script>"))
	if end != 0 {
		t.Errorf("expected the vendored document block to be empty, got %d bytes", end)
	}
}

// The rest of the pack must materialize too — the reference the SKILL.md tells
// the agent to read, and the helper it tells it to run.
func TestBentoPackAuxFilesMaterialize(t *testing.T) {
	merged := materializeBentoPack(t)

	ref, err := os.ReadFile(filepath.Join(merged, bentoReferenceRel))
	if err != nil {
		t.Fatalf("read authoring reference: %v", err)
	}
	if sum := hex.EncodeToString(sha256Sum(ref)); sum != bentoReferenceSHA256 {
		t.Errorf("authoring.md sha256 = %s, want %s (re-vendored? update NOTICE.md too)",
			sum, bentoReferenceSHA256)
	}
	// The reference is version-coupled to the shell: it must not be the repo
	// copy, which still carries the unsubstituted placeholder.
	if bytes.Contains(ref, []byte("__APP_VERSION__")) {
		t.Error("authoring.md carries an unsubstituted __APP_VERSION__; " +
			"vendor the published guide, not the repository copy")
	}

	if _, err := os.Stat(filepath.Join(merged, bentoHelperRel)); err != nil {
		t.Fatalf("stat helper script: %v", err)
	}
	for _, name := range []string{"bento-slides/SKILL.md", "bento-slides/templates/NOTICE.md"} {
		if _, err := os.Stat(filepath.Join(merged, name)); err != nil {
			t.Errorf("stat %s: %v", name, err)
		}
	}
}

// Every `skills/bento-slides/...` path the SKILL.md tells the agent to use must
// actually exist in the materialized tree. Prose that points at a renamed file
// is a silent failure: the model follows the instruction and the command 404s.
func TestBentoSkillReferencedPathsResolve(t *testing.T) {
	merged := materializeBentoPack(t)

	body, err := os.ReadFile(filepath.Join(merged, "bento-slides", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}

	// Paths as the agent sees them: relative to the workspace, where `skills`
	// is a symlink to this merged dir.
	re := regexp.MustCompile(`skills/bento-slides/[A-Za-z0-9._/-]+`)
	found := re.FindAllString(string(body), -1)
	if len(found) == 0 {
		t.Fatal("SKILL.md references no bundled files; expected at least the helper")
	}
	seen := map[string]bool{}
	for _, ref := range found {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		rel := strings.TrimPrefix(ref, "skills/")
		if _, err := os.Stat(filepath.Join(merged, rel)); err != nil {
			t.Errorf("SKILL.md references %s, which does not exist: %v", ref, err)
		}
	}
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ── bento_doc.py contract ────────────────────────────────────────────────────

// bentoHelper returns the materialized helper path, skipping when python3 is
// absent. python3 is a hard dependency of the sandbox (the file-tool seam execs
// it for every view_file/edit_file), but a bare CI shell need not have it.
func bentoHelper(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping bento_doc.py contract tests")
	}
	return filepath.Join(materializeBentoPack(t), bentoHelperRel)
}

func runHelper(t *testing.T, helper, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{helper}, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// The core contract: new -> get -> edit -> set -> get round-trips the document,
// preserves docId, and leaves every byte of the app shell untouched.
func TestBentoDocHelperRoundTrip(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()
	deck := "Q4_Review.bento.html"

	if _, stderr, err := runHelper(t, helper, dir, "new", deck); err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	if _, stderr, err := runHelper(t, helper, dir, "validate", deck); err != nil {
		t.Fatalf("validate a fresh deck: %v\n%s", err, stderr)
	}

	stdout, stderr, err := runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get: %v\n%s", err, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("get did not emit valid JSON: %v", err)
	}
	if doc["format"] != "bento/slides" {
		t.Errorf("format = %v, want bento/slides", doc["format"])
	}
	// A fresh deck must NOT carry a docId: the app mints the identity on first
	// open, and a tool inventing one would break the format invariant.
	if _, ok := doc["docId"]; ok {
		t.Error("a fresh deck must not carry a docId")
	}

	// Take on an identity the way the app would, then edit through the helper
	// and confirm the identity survives.
	doc["docId"] = "11111111-2222-3333-4444-555555555555"
	doc["title"] = "Q4 <Review> & Co"
	slides, _ := doc["slides"].([]any)
	first, _ := slides[0].(map[string]any)
	elements, _ := first["elements"].([]any)
	el, _ := elements[0].(map[string]any)
	el["html"] = "Revenue <b>up 40%</b>"
	writeJSON(t, filepath.Join(dir, "doc.json"), doc)

	if _, stderr, err := runHelper(t, helper, dir, "set", deck, "doc.json"); err != nil {
		t.Fatalf("set: %v\n%s", err, stderr)
	}

	raw, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}

	// The escaping invariant: no raw "<" survives inside the block, so it can
	// never contain a literal </script> that would end it early.
	block := docBlock(t, raw)
	if bytes.Contains(block, []byte("<")) {
		t.Error("document block contains a raw '<'; the escaping was not applied")
	}
	if !bytes.Contains(block, []byte(`\u003c`)) {
		t.Error("document block has no \u003c escape; expected the markup to be escaped")
	}

	// ... and the markup still round-trips through that escaping.
	stdout, stderr, err = runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get after set: %v\n%s", err, stderr)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(stdout), &back); err != nil {
		t.Fatalf("get after set did not emit valid JSON: %v", err)
	}
	if back["title"] != "Q4 <Review> & Co" {
		t.Errorf("title round-trip = %v", back["title"])
	}
	if back["docId"] != doc["docId"] {
		t.Errorf("docId = %v, want it preserved as %v", back["docId"], doc["docId"])
	}
	bs, _ := back["slides"].([]any)
	bf, _ := bs[0].(map[string]any)
	be, _ := bf["elements"].([]any)
	bel, _ := be[0].(map[string]any)
	if bel["html"] != "Revenue <b>up 40%</b>" {
		t.Errorf("slide markup round-trip = %v", bel["html"])
	}

	// The app shell is untouched apart from <title>, which the helper syncs
	// exactly as the app does on save.
	tpl, err := builtinSkillsFS.ReadFile(builtinSkillsRoot + "/" + bentoTemplateRel)
	if err != nil {
		t.Fatalf("read embedded template: %v", err)
	}
	if !bytes.Equal(suffixAfterBlock(t, raw), suffixAfterBlock(t, tpl)) {
		t.Error("bytes after the document block differ from the vendored shell")
	}
	// ... apart from the no-update-check guard, which `new` plants ahead of the
	// document block. Strip that one element and the vendored bytes must come
	// back exactly: the guard is the ONLY thing fleet adds to upstream's shell.
	gotPrefix := prefixAfterTitle(t, raw)
	if !bytes.Contains(gotPrefix, []byte(bentoGuardID)) {
		t.Fatalf("the guard is missing from a deck made by `new`")
	}
	if !bytes.Equal(stripGuard(t, gotPrefix), prefixAfterTitle(t, tpl)) {
		t.Error("bytes between </title> and the document block differ from the vendored shell by more than the guard")
	}
	if !bytes.Contains(raw, []byte("<title>Q4 Review & Co</title>")) {
		t.Error("shell <title> was not synced from doc.title")
	}
}

// get must never put live-session private keys in front of the model, and set
// must REMOVE the session rather than carry it across.
//
// #1197 deliberately restored collab keys from the file so that redaction could
// not destroy a user's live session. That reasoning treated a collab block as
// inert data it would be rude to drop. It is not inert: `bornWithCollab =
// !!doc.collab` makes a deck share-eligible, so a deck carrying one joins a live
// session the moment it is opened, with no click. fleet ships Bento offline-only,
// so the block goes — loudly, on stderr, because only the user can weigh it and
// because dropping the keys does not retract an invitation already handed out.
func TestBentoDocHelperStripsCollabSession(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()
	deck := "shared.bento.html"

	if _, stderr, err := runHelper(t, helper, dir, "new", deck); err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	stdout, _, err := runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Obviously-fake, low-entropy placeholder: a realistic-looking key here
	// would be a gitleaks finding in this very file.
	const fakeKey = "collab-writer-priv-placeholder-not-real"

	// Plant the session by rewriting the document block directly, the way the
	// Bento app itself would on save. It cannot be done through `set` any more —
	// that is the behavior under test.
	doc["collab"] = map[string]any{"room": "room-1", "key": "k", "on": true, "writerPriv": fakeKey}
	injectDoc(t, filepath.Join(dir, deck), doc)

	stdout, stderr, err := runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get shared deck: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, fakeKey) || strings.Contains(stderr, fakeKey) {
		t.Error("get leaked a collab private key into its output")
	}
	if !strings.Contains(stderr, "live-collaboration") {
		t.Errorf("get did not warn that the deck is shared; stderr = %q", stderr)
	}
	var redacted map[string]any
	if err := json.Unmarshal([]byte(stdout), &redacted); err != nil {
		t.Fatalf("get output: %v", err)
	}
	if _, ok := redacted["collab"]; ok {
		t.Error("get emitted a collab block; it must be stripped")
	}

	// Writing the document back must remove the session entirely — and say so,
	// since only the user can decide what to do about an invitation already out.
	redacted["title"] = "Revised"
	writeJSON(t, filepath.Join(dir, "doc2.json"), redacted)
	_, stderr, err = runHelper(t, helper, dir, "set", deck, "doc2.json")
	if err != nil {
		t.Fatalf("set redacted doc: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "removed this deck's live-collaboration block") {
		t.Errorf("set removed the session without reporting it; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "Rotate keys") {
		t.Errorf("set did not say that removal does not retract a shared invitation; stderr = %q", stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}
	if bytes.Contains(raw, []byte(fakeKey)) {
		t.Error("the deck still contains a collab private key after set")
	}
	var onDisk map[string]any
	if err := json.Unmarshal(unescapeBlock(docBlock(t, raw)), &onDisk); err != nil {
		t.Fatalf("parse block: %v", err)
	}
	if onDisk["title"] != "Revised" {
		t.Errorf("title = %v, want the edit applied", onDisk["title"])
	}
	if _, ok := onDisk["collab"]; ok {
		t.Error("set wrote a collab block; an offline-only deck must have none")
	}

	// The same must hold when the INCOMING document supplies a session of its
	// own — a model that copied one across, or a doc lifted from a shared deck.
	redacted["collab"] = map[string]any{"room": "room-2", "key": "k", "on": true}
	writeJSON(t, filepath.Join(dir, "doc3.json"), redacted)
	if _, stderr, err = runHelper(t, helper, dir, "set", deck, "doc3.json"); err != nil {
		t.Fatalf("set doc carrying its own collab: %v\n%s", err, stderr)
	}
	raw, err = os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("re-read deck: %v", err)
	}
	if err := json.Unmarshal(unescapeBlock(docBlock(t, raw)), &onDisk); err != nil {
		t.Fatalf("parse block: %v", err)
	}
	if _, ok := onDisk["collab"]; ok {
		t.Error("set honored a collab block supplied by the incoming document")
	}
}

// Every refusal must leave the target byte-identical. A helper that half-writes
// a deck is worse than one that declines.
func TestBentoDocHelperFailsClosed(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()
	deck := "deck.bento.html"

	if _, stderr, err := runHelper(t, helper, dir, "new", deck); err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	stdout, _, err := runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var good map[string]any
	if err := json.Unmarshal([]byte(stdout), &good); err != nil {
		t.Fatalf("get: %v", err)
	}

	withoutKey := func(key string) map[string]any {
		out := map[string]any{}
		for k, v := range good {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	mutated := func(fn func(map[string]any)) map[string]any {
		raw, _ := json.Marshal(good)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		fn(out)
		return out
	}

	cases := []struct {
		name string
		doc  any
		text string
	}{
		{"malformed json", nil, "{not json"},
		{"not an object", nil, `["a slide"]`},
		{"missing format", withoutKey("format"), ""},
		{"missing theme", withoutKey("theme"), ""},
		{"missing size", withoutKey("size"), ""},
		{"empty slides", mutated(func(d map[string]any) { d["slides"] = []any{} }), ""},
		{"wrong format", mutated(func(d map[string]any) { d["format"] = "bento/spaces" }), ""},
		{"unknown transition", mutated(func(d map[string]any) {
			d["slides"].([]any)[0].(map[string]any)["transition"] = "swoosh"
		}), ""},
		{"duplicate slide id", mutated(func(d map[string]any) {
			slides := d["slides"].([]any)
			d["slides"] = append(slides, slides[0])
		}), ""},
		{"slide missing notes", mutated(func(d map[string]any) {
			delete(d["slides"].([]any)[0].(map[string]any), "notes")
		}), ""},
	}

	before, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "candidate.json")
			if tc.doc != nil {
				writeJSON(t, path, tc.doc)
			} else if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatalf("write candidate: %v", err)
			}
			_, stderr, err := runHelper(t, helper, dir, "set", deck, "candidate.json")
			if err == nil {
				t.Fatalf("set accepted an invalid document; stderr = %q", stderr)
			}
			if !strings.Contains(stderr, "error:") {
				t.Errorf("expected a diagnostic on stderr, got %q", stderr)
			}
		})
	}

	after, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused set modified the deck; every failure must leave it untouched")
	}

	// new must not clobber an existing deck.
	if _, stderr, err := runHelper(t, helper, dir, "new", deck); err == nil {
		t.Errorf("new overwrote an existing deck; stderr = %q", stderr)
	}
	// get must reject a file that is not a deck at all.
	if err := os.WriteFile(filepath.Join(dir, "plain.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("write plain.html: %v", err)
	}
	if _, stderr, err := runHelper(t, helper, dir, "get", "plain.html"); err == nil {
		t.Errorf("get accepted a non-deck file; stderr = %q", stderr)
	}
	// A deck whose block was written without escaping cannot be spliced safely.
	corrupt := append([]byte{}, before...)
	start := bytes.Index(corrupt, []byte(bentoDocAnchor)) + len(bentoDocAnchor)
	end := start + bytes.Index(corrupt[start:], []byte("</script>"))
	corrupt = append(append(append([]byte{}, corrupt[:start]...),
		[]byte(`{"format":"bento/slides","note":"<b>raw</b>"}`)...), corrupt[end:]...)
	if err := os.WriteFile(filepath.Join(dir, "corrupt.bento.html"), corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt deck: %v", err)
	}
	writeJSON(t, filepath.Join(dir, "good.json"), good)
	if _, stderr, err := runHelper(t, helper, dir, "set", "corrupt.bento.html", "good.json"); err == nil {
		t.Errorf("set spliced a deck with an unescaped block; stderr = %q", stderr)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func docBlock(t *testing.T, raw []byte) []byte {
	t.Helper()
	start := bytes.Index(raw, []byte(bentoDocAnchor))
	if start < 0 {
		t.Fatal("no document block in deck")
	}
	start += len(bentoDocAnchor)
	end := bytes.Index(raw[start:], []byte("</script>"))
	if end < 0 {
		t.Fatal("unterminated document block")
	}
	return raw[start : start+end]
}

func suffixAfterBlock(t *testing.T, raw []byte) []byte {
	t.Helper()
	start := bytes.Index(raw, []byte(bentoDocAnchor)) + len(bentoDocAnchor)
	end := bytes.Index(raw[start:], []byte("</script>"))
	if end < 0 {
		t.Fatal("unterminated document block")
	}
	return raw[start+end:]
}

func prefixAfterTitle(t *testing.T, raw []byte) []byte {
	t.Helper()
	titleEnd := bytes.Index(raw, []byte("</title>"))
	if titleEnd < 0 {
		t.Fatal("no <title> in shell")
	}
	anchor := bytes.Index(raw, []byte(bentoDocAnchor))
	if anchor < 0 {
		t.Fatal("no document block in deck")
	}
	return raw[titleEnd : anchor+len(bentoDocAnchor)]
}

// injectDoc rewrites a deck's document block with the given document, applying
// the same "<" escaping the app and the helper both use. It exists so a test can
// build a deck that ARRIVES in a state the helper refuses to write — a shared
// deck carrying a live session, which is exactly how one reaches a user.
func injectDoc(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	escaped := bytes.ReplaceAll(payload, []byte("<"), []byte(`\u003c`))
	start := bytes.Index(raw, []byte(bentoDocAnchor))
	if start < 0 {
		t.Fatal("no document block in deck")
	}
	start += len(bentoDocAnchor)
	end := bytes.Index(raw[start:], []byte("</script>"))
	if end < 0 {
		t.Fatal("unterminated document block")
	}
	out := append([]byte{}, raw[:start]...)
	out = append(out, escaped...)
	out = append(out, raw[start+end:]...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write deck: %v", err)
	}
}

// stripGuard removes the single no-update-check <script> element the helper
// plants, so what remains can be compared against the pristine vendored shell.
func stripGuard(t *testing.T, raw []byte) []byte {
	t.Helper()
	// The guard is a CSP <meta> followed by a <script>; strip from the meta
	// through the script's close tag.
	open := bytes.Index(raw, []byte(`<meta http-equiv="Content-Security-Policy"`))
	if open < 0 {
		t.Fatal("no guard CSP meta to strip")
	}
	rest := raw[open:]
	end := bytes.Index(rest, []byte("</script>"))
	if end < 0 {
		t.Fatal("guard element is unterminated")
	}
	// Also swallow the whitespace the guard carries so the shell's own
	// indentation of the document block is restored byte for byte.
	tail := rest[end+len("</script>"):]
	trimmed := bytes.TrimLeft(tail, "\n\r\t ")
	return append(append([]byte{}, raw[:open]...), trimmed...)
}

// unescapeBlock turns the block's \u003c sequences back into "<". Go's json
// decoder already understands the escape, so this exists only to keep the
// assertions above readable about what the file actually holds.
func unescapeBlock(block []byte) []byte {
	return bytes.ReplaceAll(block, []byte(`\u003c`), []byte("<"))
}

// A deck the user cannot download is indistinguishable from a broken feature,
// and the way it breaks is silent: the file is written correctly, the reply
// links a path that does not match it, and the workspace proxy 404s (the browser
// says "file wasn't available on site"). The helper therefore PRINTS the exact
// markdown link, and these tests hold both halves of that contract.
func TestBentoDocHelperPrintsMatchingDownloadLink(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()

	for _, tc := range []struct{ name, deck, wantHref string }{
		{"workspace root", "Q4_Review.bento.html", "Q4_Review.bento.html"},
		{"subdirectory", "decks/Nested.bento.html", "decks/Nested.bento.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runHelper(t, helper, dir, "new", tc.deck)
			if err != nil {
				t.Fatalf("new: %v\n%s", err, stderr)
			}
			// The href must be the path relative to the workspace, NOT the
			// basename — that mismatch is the whole bug this guards.
			want := "(" + tc.wantHref + ")"
			if !strings.Contains(stdout, want) {
				t.Errorf("new did not print a link with href %q; got:\n%s", tc.wantHref, stdout)
			}
			if !strings.Contains(stdout, "download link") {
				t.Errorf("new did not print a download link line; got:\n%s", stdout)
			}
			// set and validate must agree with new.
			if _, _, err := runHelper(t, helper, dir, "get", tc.deck, "-o", "d.json"); err != nil {
				t.Fatalf("get: %v", err)
			}
			for _, sub := range [][]string{{"set", tc.deck, "d.json"}, {"validate", tc.deck}} {
				out, errOut, err := runHelper(t, helper, dir, sub...)
				if err != nil {
					t.Fatalf("%s: %v\n%s", sub[0], err, errOut)
				}
				if !strings.Contains(out, want) {
					t.Errorf("%s printed a link disagreeing with new; got:\n%s", sub[0], out)
				}
			}
		})
	}
}

// Names that survive the filesystem but break a markdown link must be refused at
// creation, not discovered by the user on a dead download.
func TestBentoDocHelperRejectsUndownloadableNames(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()

	for _, tc := range []struct{ name, deck string }{
		{"space", "Q4 Review.bento.html"},
		{"fragment", "Team#1.bento.html"},
		{"parens", "Deck(final).bento.html"},
		{"question mark", "What?.bento.html"},
		{"wrong extension", "deck.html"},
		{"absolute path", "/tmp/deck.bento.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := runHelper(t, helper, dir, "new", tc.deck)
			if err == nil {
				t.Fatalf("new accepted %q, which cannot be linked for download", tc.deck)
			}
			if !strings.Contains(stderr, "error:") {
				t.Errorf("expected a diagnostic naming the problem, got %q", stderr)
			}
		})
	}
}

// fleet ships Bento as a strictly OFFLINE viewer/editor, and two upstream
// behaviors would make a deck a network client instead: the launch update check,
// and live collaboration. The second is the sharper one — `bornWithCollab =
// !!doc.collab` is enough to make a deck share-eligible, so a deck that merely
// CARRIES a collab object opens a wss://sync.bento.page session on load, with no
// click, and retries. `new` plants a two-layer guard.
//
// These assertions are structural: Go tests cannot prove what a browser does on
// the wire. What they pin is the part that would rot silently — that both layers
// are present, that the browser-enforced one actually names connect-src, that
// they run ahead of the runtime they constrain, and that editing a deck neither
// drops nor duplicates them. The wire behavior was verified by hand in Chromium;
// see templates/NOTICE.md for what was run and what it showed.
func TestBentoDeckIsOfflineOnly(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()
	deck := "Guarded.bento.html"

	stdout, stderr, err := runHelper(t, helper, dir, "new", deck)
	if err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "no live collaboration") {
		t.Errorf("new did not report the deck as offline-only; stdout:\n%s", stdout)
	}

	raw, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("read deck: %v", err)
	}

	if n := bytes.Count(raw, []byte(bentoGuardID)); n != 1 {
		t.Fatalf("guard id appears %d times, want exactly 1", n)
	}

	// Layer 1 — the browser-enforced boundary. This is the one that holds when
	// localStorage is unavailable, so a deck without it is only as private as the
	// app's own cooperation.
	csp := bytes.Index(raw, []byte(`<meta http-equiv="Content-Security-Policy"`))
	if csp < 0 {
		t.Fatal("no CSP meta in the guard")
	}
	cspEnd := bytes.IndexByte(raw[csp:], '>')
	if cspEnd < 0 {
		t.Fatal("CSP meta is unterminated")
	}
	policy := string(raw[csp : csp+cspEnd])
	// connect-src is what stops both the update check and the collab socket;
	// the rest turn SKILL.md's "never author this" rules into browser rules.
	for _, directive := range []string{
		"connect-src 'none'",
		"object-src 'none'",
		"frame-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(policy, directive) {
			t.Errorf("CSP is missing %q; policy = %s", directive, policy)
		}
	}
	// A remote image is a beacon: it reports who opened the deck, and when.
	if strings.Contains(policy, "img-src") && strings.Contains(policy, "img-src *") {
		t.Error("CSP allows remote images, which report that the deck was opened")
	}

	// Layer 2 — upstream's own offline switch, so the app refuses network at its
	// own chokepoints and never attaches a collab transport, rather than
	// retrying into the CSP wall.
	if !bytes.Contains(raw, []byte(`localStorage.setItem("bento-offline", "on")`)) {
		t.Error("guard does not engage upstream's offline mode")
	}
	if !bytes.Contains(raw, []byte(`localStorage.setItem("bento-auto-check", "off")`)) {
		t.Error("guard does not turn upstream's auto-check preference off")
	}
	// Layer 2 must not be load-bearing on its own: a browser that refuses
	// localStorage for file:// URLs has to leave the deck just as offline.
	if !bytes.Contains(raw, []byte("catch (e)")) {
		t.Error("guard's storage write is not guarded against a browser that refuses localStorage")
	}

	// Ordering is the assertion that matters most: either layer planted after the
	// runtime would parse, look correct in review, and do nothing.
	guardAt := bytes.Index(raw, []byte(bentoGuardID))
	docAt := bytes.Index(raw, []byte(bentoDocAnchor))
	runtimeAt := bytes.Index(raw, []byte(`id="bento-rt"`))
	if runtimeAt < 0 {
		t.Fatal("no runtime block in the shell")
	}
	if csp >= docAt || guardAt >= docAt || docAt >= runtimeAt {
		t.Errorf("both guard layers must precede the document block and the runtime; got csp=%d guard=%d doc=%d runtime=%d",
			csp, guardAt, docAt, runtimeAt)
	}

	// An edit must neither drop the guard nor add a second copy: it lives in the
	// prefix, which `set` copies through untouched.
	if _, stderr, err = runHelper(t, helper, dir, "get", deck, "-o", "doc.json"); err != nil {
		t.Fatalf("get: %v\n%s", err, stderr)
	}
	if _, stderr, err = runHelper(t, helper, dir, "set", deck, "doc.json"); err != nil {
		t.Fatalf("set: %v\n%s", err, stderr)
	}
	after, err := os.ReadFile(filepath.Join(dir, deck))
	if err != nil {
		t.Fatalf("re-read deck: %v", err)
	}
	if n := bytes.Count(after, []byte(bentoGuardID)); n != 1 {
		t.Errorf("after an edit the guard id appears %d times, want exactly 1", n)
	}
	if n := bytes.Count(after, []byte("Content-Security-Policy")); n != 1 {
		t.Errorf("after an edit the CSP meta appears %d times, want exactly 1", n)
	}
}

// The app measures text for real and reports overflow from
// window.bento.validate(). An agent composing a deck in a chat turn has no
// browser, so `validate` carries a rough greedy-wrap estimate instead — the only
// overflow check available at authoring time. A heading that wraps one line
// further than expected collides with whatever is under it, and nothing in the
// JSON shows it.
//
// The estimate is calibrated against a real Chromium measurement: the case below
// is the one this skill's own first deck tripped, where the browser reported
// "needs 219px but the box is 180px tall". It must fire there and stay quiet
// when the box has room, or it is either useless or noise.
func TestBentoValidateFlagsLikelyTextOverflow(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()
	deck := "Fit.bento.html"

	if _, stderr, err := runHelper(t, helper, dir, "new", deck); err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	stdout, _, err := runHelper(t, helper, dir, "get", deck)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("get: %v", err)
	}
	slides, _ := doc["slides"].([]any)
	first, _ := slides[0].(map[string]any)
	elements, _ := first["elements"].([]any)
	el, _ := elements[0].(map[string]any)

	// Chromium measured this exact element as needing 219px.
	el["html"] = "Q3 in three moves."
	el["fontSize"] = 104
	el["w"] = 1000
	el["lineHeight"] = 1.05
	el["h"] = 180
	writeJSON(t, filepath.Join(dir, "doc.json"), doc)
	if _, stderr, err := runHelper(t, helper, dir, "set", deck, "doc.json"); err != nil {
		t.Fatalf("set: %v\n%s", err, stderr)
	}
	stdout, stderr, err := runHelper(t, helper, dir, "validate", deck)
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "may overflow its box") {
		t.Errorf("validate missed a text element the browser measures as overflowing;\nstdout:\n%s", stdout)
	}

	// Give it the room the browser said it needs; the advisory must go quiet.
	el["h"] = 240
	writeJSON(t, filepath.Join(dir, "doc2.json"), doc)
	if _, stderr, err := runHelper(t, helper, dir, "set", deck, "doc2.json"); err != nil {
		t.Fatalf("set roomy: %v\n%s", err, stderr)
	}
	stdout, stderr, err = runHelper(t, helper, dir, "validate", deck)
	if err != nil {
		t.Fatalf("validate roomy: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, "may overflow its box") {
		t.Errorf("validate flagged a box with room to spare;\nstdout:\n%s", stdout)
	}
}

// The SKILL.md tells the agent to hand over a PDF by naming a specific button in
// the vendored app, and to say that no PowerPoint export exists. Both claims are
// about UPSTREAM's UI, so both can rot on a re-vendor without anyone noticing —
// the prose would keep reading fine while pointing at a button that is gone.
//
// This inflates the shell's compressed runtime and checks the claims against it.
func TestBentoSkillPdfHandoverMatchesTheApp(t *testing.T) {
	tpl, err := builtinSkillsFS.ReadFile(builtinSkillsRoot + "/" + bentoTemplateRel)
	if err != nil {
		t.Fatalf("read embedded template: %v", err)
	}
	runtime := inflateBentoRuntime(t, tpl)

	skill, err := builtinSkillsFS.ReadFile(builtinSkillsRoot + "/bento-slides/SKILL.md")
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}

	// The button the handover instruction names must exist in the app.
	const pdfButton = "Export PDF (print)"
	if !bytes.Contains(runtime, []byte(pdfButton)) {
		t.Errorf("the app no longer has a %q button; SKILL.md's PDF handover instruction now points at nothing", pdfButton)
	}
	if !bytes.Contains(skill, []byte(pdfButton)) {
		t.Errorf("SKILL.md does not name the %q button, so the agent cannot tell the user where to click", pdfButton)
	}

	// ... and the claim that there is no PowerPoint export must stay true. If a
	// future Bento gains one, this fails and the guidance gets revisited rather
	// than quietly understating what the app can do.
	for _, marker := range []string{"pptx", "openxmlformats-officedocument.presentationml"} {
		if bytes.Contains(bytes.ToLower(runtime), []byte(marker)) {
			t.Errorf("the vendored app appears to export PowerPoint (%q found); SKILL.md says it cannot", marker)
		}
	}
}

// inflateBentoRuntime pulls the shell's DEFLATE-compressed runtime out of its
// base64 script block and inflates it, so a test can assert against the app's
// real strings rather than against the 689KB wrapper.
func inflateBentoRuntime(t *testing.T, shell []byte) []byte {
	t.Helper()
	// The exact tag, so the sibling `id="bento-rt-css"` block cannot match.
	const anchor = `<script id="bento-rt" type="bento/deflate-b64">`
	start := bytes.Index(shell, []byte(anchor))
	if start < 0 {
		t.Fatal("no runtime block in the shell")
	}
	start += len(anchor)
	end := bytes.Index(shell[start:], []byte("</script>"))
	if end < 0 {
		t.Fatal("runtime block is unterminated")
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(shell[start : start+end])))
	if err != nil {
		t.Fatalf("runtime block is not base64: %v", err)
	}
	out, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil && len(out) == 0 {
		t.Fatalf("inflate runtime: %v", err)
	}
	if len(out) < 500_000 {
		t.Fatalf("inflated runtime is only %d bytes; the extraction is probably wrong", len(out))
	}
	return out
}

// A deck the USER brought us keeps upstream's shell byte for byte — we do not
// rewrite someone else's file. `validate` is what surfaces the difference, so
// the agent can tell them instead of silently editing.
func TestBentoUnguardedShellIsReportedNotRewritten(t *testing.T) {
	helper := bentoHelper(t)
	dir := t.TempDir()

	tpl, err := builtinSkillsFS.ReadFile(builtinSkillsRoot + "/" + bentoTemplateRel)
	if err != nil {
		t.Fatalf("read embedded template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Theirs.bento.html"), tpl, 0o600); err != nil {
		t.Fatalf("write their deck: %v", err)
	}

	if _, stderr, err := runHelper(t, helper, dir, "new", "Ours.bento.html"); err != nil {
		t.Fatalf("new: %v\n%s", err, stderr)
	}
	if _, stderr, err := runHelper(t, helper, dir, "get", "Ours.bento.html", "-o", "doc.json"); err != nil {
		t.Fatalf("get: %v\n%s", err, stderr)
	}
	if _, stderr, err := runHelper(t, helper, dir, "set", "Theirs.bento.html", "doc.json"); err != nil {
		t.Fatalf("set on their deck: %v\n%s", err, stderr)
	}

	after, err := os.ReadFile(filepath.Join(dir, "Theirs.bento.html"))
	if err != nil {
		t.Fatalf("read their deck: %v", err)
	}
	if bytes.Contains(after, []byte(bentoGuardID)) {
		t.Error("set injected the guard into a deck we did not create; the shell must be preserved")
	}

	stdout, stderr, err := runHelper(t, helper, dir, "validate", "Theirs.bento.html")
	if err != nil {
		t.Fatalf("validate their deck: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "no fleet offline guard") {
		t.Errorf("validate did not flag an unguarded shell; stdout:\n%s", stdout)
	}

	stdout, stderr, err = runHelper(t, helper, dir, "validate", "Ours.bento.html")
	if err != nil {
		t.Fatalf("validate our deck: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, "no fleet offline guard") {
		t.Errorf("validate flagged a guarded deck as unguarded; stdout:\n%s", stdout)
	}
}

// The SKILL.md's own examples must be self-consistent: the deck path it tells the
// agent to CREATE has to be the path it tells the agent to LINK. They disagreed
// once (created in `decks/`, linked bare) and every deck the model built by
// following the example literally was undownloadable.
func TestBentoSkillExamplePathsAgree(t *testing.T) {
	merged := materializeBentoPack(t)
	body, err := os.ReadFile(filepath.Join(merged, "bento-slides", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	doc := string(body)

	newRe := regexp.MustCompile(`bento_doc\.py new ([^\s]+\.bento\.html)`)
	created := newRe.FindStringSubmatch(doc)
	if created == nil {
		t.Fatal("SKILL.md has no `bento_doc.py new <deck>` example to check")
	}

	linkRe := regexp.MustCompile(`\]\(([^)]+\.bento\.html)\)`)
	links := linkRe.FindAllStringSubmatch(doc, -1)
	if len(links) == 0 {
		t.Fatal("SKILL.md shows no markdown download link example")
	}
	for _, m := range links {
		if m[1] != created[1] {
			t.Errorf("SKILL.md creates %q but links %q — the link must be the deck's "+
				"path relative to the workspace, or the download 404s",
				created[1], m[1])
		}
	}
}
