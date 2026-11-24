package tools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// conversationPathUses lists every place outside tests that refers to
// WorkspaceDirForConversation or EnsureWorkspaceDir — the two helpers that
// return a conversation directory as a PATH — with how many references it
// holds and the reason the path is safe there. A path built from a
// conversation id names whatever is at that name now, and a sandbox can rename
// the directory and leave a symbolic link in its place, so host-side code must
// never read, list, write, chmod, copy or remove through it: it opens the
// directory with OpenConversationDir / EnsureConversationDir (or
// OpenDirNoFollow on that path) and works through the returned WorkspaceDir.
//
// TestConversationPathUsesAreReviewed fails on a reference missing from this
// list, on a second reference added to a listed function, and on an entry
// whose references are gone, so every new use is reviewed against that rule
// instead of quietly following a link. A reference is any use of either name —
// a call, a function value, a package-level initializer — through the
// internal/tools import under whatever name a file gives it, or bare inside
// package tools.
//
// Not covered: code that builds the path itself (filepath.Join(root, convID))
// without either helper. That is a review matter.
var conversationPathUses = map[string]reviewedUse{
	// The path is handed to the sandbox, a prompt or a message — resolved in
	// the sandbox's own mount namespace, or not opened at all.
	"internal/agent/manager.go:Manager.configureTurnWorkspace":                {2, "EnsureWorkspaceDir refuses a link; the path binds the sandbox file-op root and the turn's MCP scope"},
	"internal/agent/prompt.go:writeWorkingDirectory":                          {1, "prompt text only; its parent is looked up in the startup record (worktree.Pin), not opened"},
	"internal/httpapi/approval_execute.go:Server.approvalMCPScope":            {1, "checked with OpenConversationDir first; the path goes to the connector scope"},
	"internal/httpapi/approval_execute.go:Server.resolveStagedFileReferences": {1, "BindFileOpRoot: resolved inside the sandbox"},
	"internal/tools/bash.go:resolveBashWorkingDir":                            {1, "EnsureWorkspaceDir refuses a link; the path is the sandbox working directory"},
	"internal/tools/python_repl.go:runPythonWithSandbox":                      {1, "EnsureWorkspaceDir refuses a link; the path is the sandbox working directory"},
	"internal/tools/workspace.go:resolveWorkspacePath":                        {3, "EnsureWorkspaceDir refuses a link; the path is file-tool input executed in the sandbox. Exception (ADR-0036 2026-11-18): pathsec.ValidatePath/ValidatePathForRead then EvalSymlinks and os.Stat it on the host; nothing is opened or returned"},
	"internal/tools/download_url.go:resolveDownloadDir":                       {4, "path is input to a sandbox file op"},
	"internal/tools/fs.go:fileOpRoot":                                         {1, "path is input to a sandbox file op"},
	"internal/tools/workspace_reader.go:newVerifiedWorkspaceReader":           {1, "the reader goes through the sandbox"},
	"internal/tools/email_materialize.go:attachmentBasesFor":                  {1, "lexical base only; files are opened through OpenConversationDir"},
	"internal/tools/workspace.go:EnsureWorkspaceDir":                          {2, "returns the path after EnsureConversationDir succeeded (or alongside its error)"},

	// The workspace ROOT (empty id): operator configuration, not a conversation.
	"internal/httpapi/admin_chats.go:Server.adminPurge":                   {1, "workspace root (empty id), for the orphan sweep after a permanent delete"},
	"internal/httpapi/admin_storage.go:Server.handleAdminStorage":         {1, "workspace root (empty id)"},
	"internal/httpapi/admin_storage.go:Server.handleAdminStorageCleanup":  {1, "workspace root (empty id)"},
	"internal/httpapi/maintenance.go:Server.maintenancePass":              {3, "workspace root (empty id)"},
	"internal/httpapi/workspace_file.go:Server.privateWorkspaceStashRoot": {1, "workspace root (empty id)"},
	"internal/sched/handlers/workspace.go:Handlers.loadWorkspaceTask":     {1, "workspace root (empty id) for OpenRunDir"},

	// The path is opened with OpenDirNoFollow by the callee.
	"internal/httpapi/chat.go:Server.startTurn":                      {1, "appendWorkspaceInventoryBlock opens it with OpenDirNoFollow"},
	"internal/httpapi/webhook.go:Server.postWebhook":                 {1, "appendWorkspaceInventoryBlock opens it with OpenDirNoFollow"},
	"internal/httpapi/context_handles.go:Server.applyContextHandles": {1, "expandFileHandle opens it with OpenDirNoFollow"},
	"internal/httpapi/team_files.go:Server.handleTeamFile":           {1, "openWorkspaceFileNoFollow opens it with OpenDirNoFollow"},
	"internal/httpapi/team_files.go:copySharedOutputsIntoBranch":     {1, "openWorkspaceFileNoFollow opens it with OpenDirNoFollow (the path also names the copy source in a test seam)"},
	"internal/httpapi/attachments.go:stageAttachmentsIntoWorkspace":  {1, "path for the prompt block; writes go through EnsureConversationDir"},
	"internal/httpapi/workspace_file.go:Server.handleWorkspaceFile":  {1, "compared only; the directory is opened with OpenConversationDir"},
}

// reviewedUse is one conversationPathUses entry: the number of references the
// function (or a file's package-level declarations) holds, and why each is
// safe.
type reviewedUse struct {
	n   int
	why string
}

// conversationPathNames are the path-returning helpers the guard tracks.
var conversationPathNames = map[string]bool{"WorkspaceDirForConversation": true, "EnsureWorkspaceDir": true}

const toolsImportPath = "github.com/ElcanoTek/fleet/internal/tools"

// scanConversationPathUses counts the references to conversationPathNames in
// one parsed file, keyed "<rel>:<function>" ("<rel>:package-level" for
// package-level declarations). inTools says the file belongs to package
// tools, where the names are referenced bare.
func scanConversationPathUses(file *ast.File, rel string, inTools bool) map[string]int {
	local := map[string]bool{} // names this file binds to the internal/tools import
	bare := inTools
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != toolsImportPath {
			continue
		}
		switch {
		case spec.Name == nil:
			local["tools"] = true
		case spec.Name.Name == ".":
			bare = true
		case spec.Name.Name != "_":
			local[spec.Name.Name] = true
		}
	}
	// Identifiers that are not references: a selector's Sel (judged with its
	// selector), and names being declared — functions, fields, variables,
	// constants, types.
	notRefs := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			notRefs[x.Sel] = true
		case *ast.FuncDecl:
			notRefs[x.Name] = true
		case *ast.Field:
			for _, id := range x.Names {
				notRefs[id] = true
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				notRefs[id] = true
			}
		case *ast.TypeSpec:
			notRefs[x.Name] = true
		}
		return true
	})
	found := map[string]int{}
	for _, decl := range file.Decls {
		scope := "package-level"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			scope = fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				scope = receiverName(fn.Recv.List[0].Type) + "." + scope
			}
		}
		key := rel + ":" + scope
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && local[id.Name] && conversationPathNames[x.Sel.Name] {
					found[key]++
				}
			case *ast.Ident:
				if bare && !notRefs[x] && conversationPathNames[x.Name] {
					found[key]++
				}
			}
			return true
		})
	}
	return found
}

func TestConversationPathUsesAreReviewed(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repo, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(repo, path)
			rel = filepath.ToSlash(rel)
			inTools := filepath.ToSlash(filepath.Dir(rel)) == "internal/tools"
			for key, n := range scanConversationPathUses(file, rel, inTools) {
				found[key] += n
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var problems []string
	for key, n := range found {
		want, ok := conversationPathUses[key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("unreviewed use of a conversation-directory path: %s (%d reference(s))", key, n))
		case n != want.n:
			problems = append(problems, fmt.Sprintf("%s holds %d reference(s) to a conversation-directory path, the review covers %d", key, n, want.n))
		}
	}
	for key := range conversationPathUses {
		if _, ok := found[key]; !ok {
			problems = append(problems, fmt.Sprintf("conversationPathUses lists %s, which no longer uses either helper: remove the entry", key))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p + "\n\tHost-side work inside a conversation's workspace must open it with tools.OpenConversationDir /\n" +
			"\tEnsureConversationDir (no symbolic link followed) and go through the returned WorkspaceDir. If this\n" +
			"\tuse only builds a path for the sandbox, a prompt or a message, list it in conversationPathUses with the reason.")
	}
}

// The scanner sees a renamed import, a function value, package-level
// initializers and repeated references, and inside package tools the bare
// names — but not the helpers' own declarations.
func TestScanConversationPathUsesShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		inTools bool
		want    map[string]int
	}{
		{"renamed import, function value, package-level, repeats", `package x
import wt "github.com/ElcanoTek/fleet/internal/tools"
var dirFor = wt.WorkspaceDirForConversation
var root = wt.WorkspaceDirForConversation("")
type S struct{}
func (S) f(id string) string {
	ensure := wt.EnsureWorkspaceDir
	_ = ensure
	return wt.WorkspaceDirForConversation(id) + wt.WorkspaceDirForConversation(id)
}
`, false, map[string]int{"x.go:package-level": 2, "x.go:S.f": 3}},
		{"default import name", `package x
import "github.com/ElcanoTek/fleet/internal/tools"
func g(id string) string { return tools.WorkspaceDirForConversation(id) }
`, false, map[string]int{"x.go:g": 1}},
		{"another package's tools import is not this one", `package x
import tools "example.com/other/tools"
func g(id string) string { return tools.WorkspaceDirForConversation(id) }
`, false, map[string]int{}},
		{"dot import", `package x
import . "github.com/ElcanoTek/fleet/internal/tools"
var h = EnsureWorkspaceDir
`, false, map[string]int{"x.go:package-level": 1}},
		{"bare names inside package tools, declarations excluded", `package tools
var h = WorkspaceDirForConversation
func WorkspaceDirForConversation(id string) string { return id }
func EnsureWorkspaceDir(id string) (string, error) { return WorkspaceDirForConversation(id), nil }
func k(d struct{ WorkspaceDirForConversation int }) { _ = EnsureWorkspaceDir; _ = d.WorkspaceDirForConversation }
`, true, map[string]int{"x.go:package-level": 1, "x.go:EnsureWorkspaceDir": 1, "x.go:k": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := scanConversationPathUses(file, "x.go", tc.inTools)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("found %v, want %v", got, tc.want)
			}
		})
	}
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiverName(e.X)
	}
	return "?"
}
