package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/apispec"
	"github.com/ElcanoTek/fleet/internal/apispec/orchestrator"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// updateOpenAPI regenerates docs/openapi.yaml instead of comparing it:
//
//	go test ./cmd/fleet -run TestOpenAPIGenerated -update
//
// (`make api-contracts` runs exactly that, then regenerates the web types).
var updateOpenAPI = flag.Bool("update", false, "rewrite docs/openapi.yaml from the orchestrator router and internal/apispec/orchestrator")

// TestOpenAPIGenerated is the drift gate for the generated contract
// (ADR-0109): docs/openapi.yaml must equal what internal/apispec generates
// from the REAL orchestrator router plus the route catalog. Generation itself
// fails when a registered route has no catalog entry or a catalog entry names
// a route the router lacks, so a route cannot be added, renamed or removed
// without the contract following.
func TestOpenAPIGenerated(t *testing.T) {
	h := handlers.New(handlers.Config{}, nil, nil)
	notes := handlers.NewNotesHandlers(nil, h)
	router, ok := buildOrchestratorMux(h, notes, reloadConfigHandler(nil), mcpReloadHandler(nil), nil).(chi.Routes)
	if !ok {
		t.Fatal("orchestrator router is not chi.Routes")
	}
	routes, err := apispec.WalkChi(router)
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	want, err := apispec.Generate(orchestrator.Family(), routes)
	if err != nil {
		t.Fatal(err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "openapi.yaml")
	if *updateOpenAPI {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("docs/openapi.yaml is out of date with the router and internal/apispec/orchestrator — run `make api-contracts` and commit the result")
	}
}

// TestOpenAPITaskStatusEnum pins Task.status's enum to models.AllTaskStatuses.
// The schema-drift gate checks property names and types, not enum values, and
// the hand-written enum had drifted to six statuses, two of which (completed,
// failed) no task row can hold, while missing six the server does report;
// the web types generated from the spec are only as right as this list.
func TestOpenAPITaskStatusEnum(t *testing.T) {
	t.Parallel()
	var doc struct {
		Components struct {
			Schemas struct {
				Task struct {
					Properties struct {
						Status struct {
							Enum []string `yaml:"enum"`
						} `yaml:"status"`
					} `yaml:"properties"`
				} `yaml:"Task"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(models.AllTaskStatuses))
	for _, s := range models.AllTaskStatuses {
		want = append(want, string(s))
	}
	if got := doc.Components.Schemas.Task.Properties.Status.Enum; !slices.Equal(got, want) {
		t.Errorf("Task.status enum = %v, want models.AllTaskStatuses %v", got, want)
	}
}
