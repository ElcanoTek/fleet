// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

const (
	handlersPkgPath = "github.com/ElcanoTek/fleet/internal/sched/handlers"
	storagePkgPath  = "github.com/ElcanoTek/fleet/internal/sched/storage"
	dbPkgPath       = "github.com/ElcanoTek/fleet/internal/sched/db"
	modelsPkgPath   = "github.com/ElcanoTek/fleet/internal/sched/models"
)

// Every *storage.Storage method a handler refers to is classified: a create
// seam, an edit seam, a write outside the gates (with the reason), a read, or
// forbidden. An unclassified method is a finding, so a new storage write
// cannot reach a handler unseen.

// taskCreateSeams are the storage methods that insert a task. A function that
// refers to one must also run Handlers.admitCreate.
var taskCreateSeams = map[string]bool{
	"AddTask":            true,
	"AddTaskWithContext": true,
	"AddTaskBatch":       true,
	"EnqueueTask":        true,
	"EnqueueTaskAs":      true,
}

// taskEditSeams are the storage methods that rewrite fields a create gate
// decides (the ceilings, run_if, priority, mcp_selection, …) on an existing
// row. A function that refers to one must also run Handlers.admitEdit.
var taskEditSeams = map[string]bool{
	"ReplaceTaskDefinition":         true,
	"UpdateEditableTask":            true,
	"UpdateTask":                    true,
	"UpdateTaskCredentialAllowlist": true,
	"UpdateTaskSandboxLimits":       true,
	"UpdateTaskDescription":         true,
}

// storageWritesOutsideTheGates are the storage writes handlers use without the
// gate functions, each with the reason. Adding one is a decision a reviewer
// should see: no create gate runs on that write.
var storageWritesOutsideTheGates = map[string]string{
	"UpdateTaskTags":               "rewrites tags only, normalized by models.NormalizeAndValidateTags in UpdateTaskTags; no create gate decides a tag",
	"SetTaskOwner":                 "admin-only reassignment (PermissionAdmin in SetTaskOwner), which runs the connector check for the new owner on every row it rewrites",
	"BulkUpdateScheduledTaskModel": "admin-only route (AdminAuthMiddleware) that rewrites the model slug only",
	"SpawnWebhookRun":              "a trigger run copies its template, whose definition was written through the gates; triggers are created only by the operator CLI",
	"SpawnEmailRun":                "a trigger run copies its template, whose definition was written through the gates; triggers are created only by the operator CLI",
	"SetTriggerEventRunID":         "links a trigger event to the run it spawned; writes no task field",
	"RecordTriggerEvent":           "records an inbound trigger event; writes no task",
	"CancelTaskAtomic":             "stops a task: its status, no definition field",
	"DeleteTask":                   "removes a task",
	"ResumeTask":                   "hands a paused run its answer: status and pending_answer, no definition field",
	"WakeTaskByEvent":              "wakes a task parked on an event: its wake columns, no definition field",
	"CleanupHistory":               "admin-only route: deletes finished runs",
	"UpsertBudget":                 "admin-only: writes a budget row, not a task",
	"DeleteBudget":                 "admin-only: deletes a budget row, not a task",
	"CreatePromptLibrary":          "writes a prompt-library entry, not a task",
	"UpdatePromptLibrary":          "writes a prompt-library entry, not a task",
	"DeletePromptLibrary":          "deletes a prompt-library entry, not a task",
	"UpsertA2APushConfig":          "stores an A2A caller's per-task webhook registration; no task field",
	"DeleteA2APushConfig":          "deletes an A2A push-notification config; no task field",
}

// storageReadPrefixes name the storage methods that only read: a method whose
// name starts with one of them needs no entry.
var storageReadPrefixes = []string{"Get", "List", "Find", "Count", "ForEach"}

// storageReads are the other storage methods handlers use that write nothing.
var storageReads = map[string]string{
	"Location":           "returns the scheduler's default time zone",
	"Identity":           "returns the account store; an account is not a task",
	"TaskUsage":          "reads spend",
	"TaskUsageByUserDay": "reads spend",
	"PendingQueueStats":  "reads queue depth",
	"MigrationStatus":    "reads the schema version",
}

// storageForbidden are storage methods no handler may use, whatever the gate.
var storageForbidden = map[string]string{
	"DB": "returns the raw database handle, whose writes skip the storage layer and every gate",
}

// gateExemptFunctions are functions allowed to refer to a seam, or to
// models.NewTask, without running the matching gate themselves, each with the
// reason. It is empty: every function that writes a task runs the gate.
var gateExemptFunctions = map[string]string{}

// storageInsertsThatAreNotTasks are *storage.Storage methods whose name starts
// like a task insert but write no task row.
var storageInsertsThatAreNotTasks = map[string]string{
	"AddTaskIteration": "records one loop iteration of an existing task",
}

// TestTaskWritesGoThroughTheGates is the structural half of the create-gate
// guarantee. It type-checks this package and fails when:
//
//   - a function other than Handlers.admitCreate refers to models.NewTask;
//   - a function refers to a task-insert method of *storage.Storage without
//     also running Handlers.admitCreate, or to a definition-edit method
//     without Handlers.admitEdit;
//   - a function refers to a seam method name through an interface without
//     the matching gate (the interface value may be the store);
//   - a function refers to a *storage.Storage method no list classifies, to
//     a forbidden one (Storage.DB), or to any *db.Database method;
//   - a storage method that reads as a task insert is not classified.
//
// Matching is by type, on every selector rather than only on calls, so an
// alias of the store, a method value, a method expression, a promoted method
// and a helper that takes *storage.Storage are all seen
// (TestTaskGateGuardSeesThroughIndirection pins each shape).
//
// The trade-off it accepts: the gate must run in the same function as the
// write. Extracting the shared persist step into a helper, or calling
// models.NewTask for a preview that saves nothing, needs a
// gateExemptFunctions entry with the reason. It does not follow a write
// reached through reflection, or through a function in another package that
// holds the store.
func TestTaskWritesGoThroughTheGates(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	pkg, info := typeCheckForGuard(t, fset, handlersPkgPath, files)
	scan := scanTaskGates(fset, pkg, files, info)

	var problems []string
	usedExemptions := map[string]bool{}
	for _, f := range scan.findings {
		if _, ok := gateExemptFunctions[f.fn]; ok {
			usedExemptions[f.fn] = true
			continue
		}
		problems = append(problems, f.String())
	}

	// Every storage method that reads as a task insert is classified, so a
	// new insert seam cannot be added to storage and used here unseen.
	storageType := reflect.TypeOf((*storage.Storage)(nil))
	methods := map[string]bool{}
	for i := range storageType.NumMethod() {
		m := storageType.Method(i).Name
		methods[m] = true
		if strings.HasPrefix(m, "AddTask") || strings.HasPrefix(m, "EnqueueTask") {
			if _, notTask := storageInsertsThatAreNotTasks[m]; !taskCreateSeams[m] && !notTask {
				problems = append(problems, "storage."+m+" reads as a task insert: add it to taskCreateSeams, or to storageInsertsThatAreNotTasks with the reason")
			}
		}
	}
	for _, list := range []map[string]bool{taskCreateSeams, taskEditSeams} {
		for m := range list {
			if !methods[m] {
				problems = append(problems, "storage."+m+" no longer exists: remove it from the seam list")
			}
		}
	}
	// The lists name only what handlers use today, so each entry stays a
	// decision someone made about current code.
	for name, list := range map[string]map[string]string{"storageWritesOutsideTheGates": storageWritesOutsideTheGates, "storageReads": storageReads} {
		for m := range list {
			if !scan.classifiedUsed[m] {
				problems = append(problems, name+" lists storage."+m+", which no handler uses any more: remove it")
			}
		}
	}
	for fn := range gateExemptFunctions {
		if !usedExemptions[fn] {
			problems = append(problems, "gateExemptFunctions lists "+fn+", which no longer needs it: remove it")
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// gateGuardFixture holds the shapes the guard must see, plus the shapes it
// must accept (gatedCreate, a read). It is type-checked against the real
// storage and models packages.
const gateGuardFixture = `package fixture

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

type Handlers struct{ storage *storage.Storage }

type gates struct{}

func (h *Handlers) admitCreate(context.Context, *gates, *models.TaskCreate) (*models.Task, error) {
	return nil, nil
}

func (h *Handlers) admitEdit(context.Context, *gates) error { return nil }

func (h *Handlers) gatedCreate(ctx context.Context, tc *models.TaskCreate) error {
	task, err := h.admitCreate(ctx, &gates{}, tc)
	if err != nil {
		return err
	}
	_, err = h.storage.AddTaskWithContext(ctx, task)
	return err
}

func (h *Handlers) read(id uuid.UUID) (*models.Task, error) { return h.storage.GetTask(id) }

func (h *Handlers) aliasedStore(ctx context.Context, task *models.Task) {
	st := h.storage
	_, _ = st.AddTaskWithContext(ctx, task)
}

func (h *Handlers) methodValue(ctx context.Context, task *models.Task) {
	add := h.storage.AddTaskWithContext
	_, _ = add(ctx, task)
}

func persist(ctx context.Context, st *storage.Storage, task *models.Task) {
	_, _ = st.AddTaskWithContext(ctx, task)
}

func (h *Handlers) aliasedEnqueue(ctx context.Context) {
	st := h.storage
	_, _, _, _ = st.EnqueueTask(ctx, models.TaskCreate{})
}

func (h *Handlers) aliasedEdit(ctx context.Context, id uuid.UUID) {
	st := h.storage
	_, _ = st.UpdateEditableTask(ctx, id, storage.TaskEdit{})
}

func (h *Handlers) methodExpression(ctx context.Context, task *models.Task) {
	_, _ = (*storage.Storage).AddTaskWithContext(h.storage, ctx, task)
}

type embedded struct{ *storage.Storage }

func (e embedded) promoted(task *models.Task) { _, _ = e.AddTask(task) }

type enqueuer interface {
	EnqueueTask(context.Context, models.TaskCreate) (uuid.UUID, string, time.Time, error)
}

func (h *Handlers) throughInterface(ctx context.Context) {
	var e enqueuer = h.storage
	_, _, _, _ = e.EnqueueTask(ctx, models.TaskCreate{})
}

func (h *Handlers) wrongGate(ctx context.Context, task *models.Task) {
	_ = h.admitEdit(ctx, &gates{})
	_, _ = h.storage.AddTask(task)
}

func (h *Handlers) rawDatabase(ctx context.Context, task *models.Task) {
	_, _ = h.admitCreate(ctx, &gates{}, &models.TaskCreate{})
	_ = h.storage.DB().AddTask(ctx, task)
}

func (h *Handlers) mintValue() {
	mint := models.NewTask
	_ = mint(models.TaskCreate{})
}

func (h *Handlers) unclassifiedWrite() { _ = h.storage.SetTaskErrorAnalysis }

func (h *Handlers) unclassifiedReplay() { _ = h.storage.ReplayDeadLetteredTask }

var packageLevel = func(h *Handlers, task *models.Task) { _, _ = h.storage.AddTask(task) }
`

// TestTaskGateGuardSeesThroughIndirection tests the guard itself: every
// indirect way to reach a seam in gateGuardFixture is reported, and the gated
// create and the read are not.
func TestTaskGateGuardSeesThroughIndirection(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", gateGuardFixture, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	files := []*ast.File{file}
	pkg, info := typeCheckForGuard(t, fset, "example.com/fixture", files)
	got := map[string][]string{}
	for _, f := range scanTaskGates(fset, pkg, files, info).findings {
		got[f.fn] = append(got[f.fn], f.msg)
	}

	for fn, want := range map[string][]string{
		"Handlers.aliasedStore":       {"storage.AddTaskWithContext without Handlers.admitCreate"},
		"Handlers.methodValue":        {"storage.AddTaskWithContext without Handlers.admitCreate"},
		"persist":                     {"storage.AddTaskWithContext without Handlers.admitCreate"},
		"Handlers.aliasedEnqueue":     {"storage.EnqueueTask without Handlers.admitCreate"},
		"Handlers.aliasedEdit":        {"storage.UpdateEditableTask without Handlers.admitEdit"},
		"Handlers.methodExpression":   {"storage.AddTaskWithContext without Handlers.admitCreate"},
		"embedded.promoted":           {"storage.AddTask without Handlers.admitCreate"},
		"Handlers.throughInterface":   {"EnqueueTask through an interface without Handlers.admitCreate"},
		"Handlers.wrongGate":          {"storage.AddTask without Handlers.admitCreate"},
		"Handlers.rawDatabase":        {"storage.DB, which no handler may use", "db.Database.AddTask on the raw database handle"},
		"Handlers.mintValue":          {"models.NewTask"},
		"Handlers.unclassifiedWrite":  {"storage.SetTaskErrorAnalysis, which no list classifies"},
		"Handlers.unclassifiedReplay": {"storage.ReplayDeadLetteredTask, which no list classifies"},
		"var packageLevel":            {"storage.AddTask without Handlers.admitCreate"},
	} {
		msgs := strings.Join(got[fn], "\n")
		for _, w := range want {
			if !strings.Contains(msgs, w) {
				t.Errorf("%s: findings %q, want one containing %q", fn, msgs, w)
			}
		}
		delete(got, fn)
	}
	for fn, msgs := range got {
		t.Errorf("%s: unexpected findings %q", fn, msgs)
	}
}

// gateFinding is one place the guard objects to.
type gateFinding struct {
	fn  string // "Handlers.method", "func", or "var name"
	pos string // file:line
	msg string
}

func (f gateFinding) String() string { return f.pos + ": " + f.fn + " " + f.msg }

// gateScan is what scanTaskGates found: the findings, and the classified
// storage methods (outside-the-gates writes and reads) handlers use.
type gateScan struct {
	findings       []gateFinding
	classifiedUsed map[string]bool
}

// gateRef is one reference to a store method.
type gateRef struct {
	method string
	kind   gateRefKind
	pos    token.Pos
}

type gateRefKind int

const (
	refStorage   gateRefKind = iota // a *storage.Storage method
	refDatabase                     // a *db.Database method
	refInterface                    // a seam name on an interface
)

// scanTaskGates checks every function and package-level declaration in files.
func scanTaskGates(fset *token.FileSet, pkg *types.Package, files []*ast.File, info *types.Info) gateScan {
	scan := gateScan{classifiedUsed: map[string]bool{}}
	for _, file := range files {
		for _, decl := range file.Decls {
			var name string
			var node ast.Node
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				name, node = funcDeclName(d), d.Body
			case *ast.GenDecl:
				name, node = genDeclName(d), d
			default:
				continue
			}
			scan.unit(fset, pkg, info, name, node)
		}
	}
	return scan
}

func (scan *gateScan) unit(fset *token.FileSet, pkg *types.Package, info *types.Info, name string, node ast.Node) {
	var (
		refs                   []gateRef
		mints                  token.Pos
		admitCreate, admitEdit bool
	)
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			sel, ok := info.Selections[n]
			if !ok {
				return true
			}
			fn, ok := sel.Obj().(*types.Func)
			if !ok {
				return true
			}
			recv := fn.Type().(*types.Signature).Recv()
			switch {
			case recv != nil && isNamedType(recv.Type(), storagePkgPath, "Storage"):
				refs = append(refs, gateRef{method: fn.Name(), kind: refStorage, pos: n.Sel.Pos()})
			case recv != nil && isNamedType(recv.Type(), dbPkgPath, "Database"):
				refs = append(refs, gateRef{method: fn.Name(), kind: refDatabase, pos: n.Sel.Pos()})
			case recv != nil && types.IsInterface(recv.Type()) && (taskCreateSeams[fn.Name()] || taskEditSeams[fn.Name()]):
				refs = append(refs, gateRef{method: fn.Name(), kind: refInterface, pos: n.Sel.Pos()})
			case fn.Pkg() == pkg && fn.Name() == "admitCreate":
				admitCreate = true
			case fn.Pkg() == pkg && fn.Name() == "admitEdit":
				admitEdit = true
			}
		case *ast.Ident:
			if fn, ok := info.Uses[n].(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == modelsPkgPath && fn.Name() == "NewTask" {
				mints = n.Pos()
			}
		}
		return true
	})

	add := func(pos token.Pos, msg string) {
		p := fset.Position(pos)
		scan.findings = append(scan.findings, gateFinding{fn: name, pos: fmt.Sprintf("%s:%d", p.Filename, p.Line), msg: msg})
	}
	if mints.IsValid() && name != "Handlers.admitCreate" {
		add(mints, "refers to models.NewTask; mint tasks through Handlers.admitCreate")
	}
	for _, ref := range refs {
		target := "storage." + ref.method
		switch ref.kind {
		case refDatabase:
			add(ref.pos, "uses db.Database."+ref.method+" on the raw database handle: a handler writes through storage and the gates")
			continue
		case refInterface:
			target = ref.method + " through an interface"
		case refStorage:
		}
		switch {
		case taskCreateSeams[ref.method]:
			if !admitCreate {
				add(ref.pos, "uses "+target+" without Handlers.admitCreate")
			}
		case taskEditSeams[ref.method]:
			if !admitEdit {
				add(ref.pos, "uses "+target+" without Handlers.admitEdit")
			}
		case storageForbidden[ref.method] != "":
			add(ref.pos, "uses "+target+", which no handler may use: it "+storageForbidden[ref.method])
		case storageWritesOutsideTheGates[ref.method] != "" || storageReads[ref.method] != "":
			scan.classifiedUsed[ref.method] = true
		case hasAnyPrefix(ref.method, storageReadPrefixes):
		default:
			add(ref.pos, "uses "+target+", which no list classifies: run it through admitCreate or admitEdit, or add it to storageWritesOutsideTheGates or storageReads with the reason")
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// isNamedType reports whether t, or what t points to, is the named type
// pkgPath.name.
func isNamedType(t types.Type, pkgPath, name string) bool {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == name
}

func funcDeclName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	recv := d.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if idx, ok := recv.(*ast.IndexExpr); ok {
		recv = idx.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

func genDeclName(d *ast.GenDecl) string {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return d.Tok.String() + " " + s.Names[0].Name
			}
		case *ast.TypeSpec:
			return "type " + s.Name.Name
		}
	}
	return d.Tok.String()
}

// typeCheckForGuard type-checks files as package path, importing every
// dependency from the compiler's export data (go list -export), so the guard
// sees the same types the build does.
func typeCheckForGuard(t *testing.T, fset *token.FileSet, path string, files []*ast.File) (*types.Package, *types.Info) {
	t.Helper()
	exports, err := guardExportData()
	if err != nil {
		t.Fatalf("listing export data for the type check: %v", err)
	}
	imp := importer.ForCompiler(fset, "gc", func(importPath string) (io.ReadCloser, error) {
		file, ok := exports[importPath]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", importPath)
		}
		return os.Open(file)
	})
	info := &types.Info{
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Uses:       map[*ast.Ident]types.Object{},
	}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", path, err)
	}
	return pkg, info
}

// guardBuildFlags are the flags this test binary was built with that change
// how a package compiles, as its build info records them. go list runs with
// the same ones, so the export data it lists is the objects the test build
// already compiled: a -race run lists race objects, an untagged run lists
// untagged ones, and nothing is compiled a second time.
func guardBuildFlags() []string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	var flags []string
	for _, s := range info.Settings {
		switch s.Key {
		case "-race", "-msan", "-asan", "-tags", "-gcflags", "-asmflags", "-trimpath":
			flags = append(flags, s.Key+"="+s.Value)
		}
	}
	return flags
}

// guardExportData maps each dependency of this package to its export data
// file, listed once per test binary. It lists the dependencies, not the
// package itself: the guard type-checks this package from source, and
// exporting it would compile a variant the test build never made.
var guardExportData = sync.OnceValues(func() (map[string]string, error) {
	flags := guardBuildFlags()
	deps, err := goList(append(append([]string{}, flags...), "-f", `{{join .Deps "\n"}}`, ".")...)
	if err != nil {
		return nil, err
	}
	out, err := goList(append(append(append([]string{}, flags...), "-export", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}"), deps...)...)
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	for _, line := range out {
		if importPath, file, ok := strings.Cut(line, "="); ok {
			exports[importPath] = file
		}
	}
	return exports, nil
})

// goList runs go list with args and returns its non-empty output lines.
func goList(args ...string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
