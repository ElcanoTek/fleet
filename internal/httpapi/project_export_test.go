package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// The export endpoint owns its download filename, the same way conversation
// export, dataset CSV, prompts, and adoption do (#896).
//
// It previously set no Content-Disposition at all: the Next proxy synthesized
// `project-<uuid>.json` from the URL path, so the saved file was named after an
// opaque id, and the proxy funnel that was supposed to forward the header
// dropped it anyway. Moving the header here means one owner of the filename and
// a name a human recognizes.
func TestProjectExport_SetsContentDispositionFilename(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const owner = "owner@x.com"

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: owner, Name: "Q3 Planning"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	w := getProjectSub(t, srv, owner, proj.ID, "export")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	cd := w.Header().Get("Content-Disposition")
	if cd == "" {
		t.Fatal("Content-Disposition is empty; the browser would name the download after the URL path segment (\"export\")")
	}
	if !strings.HasPrefix(cd, "attachment; ") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", cd)
	}
	// The project's own name, slugified, rather than its uuid.
	if !strings.Contains(cd, "Q3-Planning") {
		t.Errorf("Content-Disposition = %q, want the project name slug %q in the filename", cd, "Q3-Planning")
	}
	if !strings.HasSuffix(cd, `.json"`) {
		t.Errorf("Content-Disposition = %q, want a .json extension", cd)
	}

	// The body is unchanged by the header work.
	var body struct {
		Version         string   `json:"version"`
		ConversationIDs []string `json:"conversation_ids"`
		Project         struct {
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal export body: %v", err)
	}
	if body.Version != "1" {
		t.Errorf("version = %q, want \"1\"", body.Version)
	}
	if body.Project.Name != "Q3 Planning" {
		t.Errorf("project.name = %q, want %q", body.Project.Name, "Q3 Planning")
	}
}

// A project name made entirely of characters exportFilename strips must still
// produce a usable filename rather than a bare extension — the header is
// attacker-adjacent only in that a user names their own project, but a
// degenerate name should not yield `attachment; filename=".json"`.
func TestProjectExport_FilenameSurvivesAnUnslugifiableName(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const owner = "owner@x.com"

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: owner, Name: `///"""`})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	w := getProjectSub(t, srv, owner, proj.ID, "export")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cd := w.Header().Get("Content-Disposition")
	// A quote surviving into the header would terminate the quoted-string early.
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition = %q: want exactly two quotes (name must not inject any)", cd)
	}
	if !strings.HasSuffix(cd, `.json"`) {
		t.Errorf("Content-Disposition = %q, want a .json extension", cd)
	}
	// Falls back to the noun being exported, not exportFilename's old hardcoded
	// "chat" — a project download named chat-a1b2c3d4.json reads as a bug.
	if !strings.Contains(cd, "project-") {
		t.Errorf("Content-Disposition = %q, want the \"project\" fallback slug", cd)
	}
}
