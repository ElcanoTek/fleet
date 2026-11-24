package hostgit

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// execSitesAllowed lists the functions ("<file>.<Func>", methods as
// "<file>.<Type>.<Method>") that start a process whose program, or whose
// shell command, is not a constant the guard can read — each with the reason
// it is not host-side git. Everything else that starts a process must name a
// constant program, and that program must not be git (or a shell or env
// running git): host-side git goes through hostgit.
var execSitesAllowed = map[string]string{
	"cmd/fleet/health.go.podmanSandboxProbe":                            "the operator-configured sandbox runtime binary (podman), `--version` only",
	"cmd/fleet/mcp_broker_runtime.go.productionMCPBrokerCommand":        "the running fleet binary itself, started as the MCP broker",
	"internal/admincli/bootstrap.go.cmdBootstrap":                       "bash running fleet's own scripts/bootstrap.sh from the operator's checkout, at the operator's request",
	"internal/admincli/cleanup.go.runLoud":                              "fixed podman/dnf/journalctl cleanup steps from a literal table",
	"internal/admincli/config_explain.go.systemctlShow":                 "systemctl resolved through PATH",
	"internal/admincli/doctor.go.cmdDoctorChecks":                       "bash running fleet's own scripts/doctor.sh from the operator's checkout, at the operator's request",
	"internal/admincli/env.go.envEdit":                                  "the operator's own $EDITOR on the env file, at the operator's request",
	"internal/admincli/release.go.checkReleaseBinary":                   "a downloaded fleet release binary after signature verification, `version --build-info` only",
	"internal/admincli/status.go.checkSSHPasswords":                     "sshd resolved through PATH, `-T` only",
	"internal/admincli/update.go.cmdUpdate":                             "bash running fleet's own scripts/update.sh, which updates the operator's checkouts (not sandbox-writable) at the operator's request",
	"internal/mcp/client.go.newStdioTransport":                          "a stdio MCP server's command from the client-config bundle: host-side connectors run by design (ADR-0036)",
	"internal/playwrightrunner/runner.go.runSubprocess":                 "python3 or node resolved through PATH, running an embedded script",
	"internal/playwrightrunner/session.go.spawnWorkerCmd":               "python3 or node resolved through PATH, running an embedded script",
	"internal/playwrightrunner/session.go.startSessionWorkerProcess":    "python3 resolved through PATH, `-c import playwright` only",
	"internal/release/cosign.go.Cosign.run":                             "the digest-checked pinned cosign binary",
	"internal/sandbox/container.go.containerImpl.close":                 "the operator-configured podman binary",
	"internal/sandbox/container.go.containerImpl.ensureBridge":          "the operator-configured podman binary (exec into the sandbox)",
	"internal/sandbox/container.go.containerImpl.executeFileOp":         "the operator-configured podman binary (exec into the sandbox)",
	"internal/sandbox/container.go.containerImpl.killContainerNow":      "the operator-configured podman binary",
	"internal/sandbox/container.go.containerImpl.reapDetachedProcesses": "the operator-configured podman binary (exec into the sandbox)",
	"internal/sandbox/container.go.containerImpl.runBash":               "the operator-configured podman binary (exec into the sandbox)",
	"internal/sandbox/container.go.containerImpl.start":                 "the operator-configured podman binary",
	"internal/sandbox/exec.go.PodmanExec.CommandContext":                "a podman argv the sandbox backend assembles",
	"internal/sandbox/host.go.hostImpl.runBash":                         "the test-only host executor (build tag fleet_host_executor) running a tool call",
	"internal/sandbox/oci_runtime.go.preflightKata":                     "PodmanExec running the Kata runtime binary from the fixed runtime-to-binary mapping, `check` only",
	"internal/sandbox/oci_runtime.go.verifyKrunLibkrun":                 "PodmanExec running the crun binary from the fixed runtime-to-binary mapping, `--version` only",
	"internal/sandbox/prewarm.go.PrewarmKeepIDImage":                    "the operator-configured podman binary",
	"internal/sandbox/prewarm.go.podmanImageID":                         "the operator-configured podman binary",
	"internal/sandbox/prune.go.PruneOrphanedContainers":                 "the operator-configured podman binary",
	"internal/sandbox/stats.go.pollOnce":                                "the operator-configured podman binary",
	"internal/sandbox/storage_probe.go.ProbeStorageOptSupport":          "the operator-configured podman binary",
	"internal/sched/scheduler/scheduler.go.Scheduler.evalRunIf":         "a task's run_if gate: an operator-authored host-side shell command by design (#269); if it runs git, that is the operator's command, not one fleet issues",
}

// TestHostGitRunsOnlyThroughThisPackage pins that host-side git runs only
// through hostgit. Outside this package it fails on:
//   - an os/exec Command/CommandContext (or LookPath) whose program is git,
//     resolved through constants (go/types), so `const bin = "git"` counts;
//   - env running git, and sh/bash/dash/zsh/ksh/ash -c with a command line
//     that runs git anywhere in it;
//   - a program, an env target or a shell command line the guard cannot
//     resolve to a constant, unless execSitesAllowed names the function;
//   - an exec.Cmd literal, os.StartProcess, syscall.Exec/ForkExec/StartProcess
//     or unix.Exec, unless execSitesAllowed names the function.
//
// Test files are out of scope: tests build their fixture repositories with
// plain git on purpose.
func TestHostGitRunsOnlyThroughThisPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	byDir := map[string][]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var bad []string
	seen := map[string]bool{}
	for dir, files := range byDir {
		relDir, _ := filepath.Rel(root, dir)
		if filepath.ToSlash(relDir) == "internal/hostgit" {
			continue
		}
		fset := token.NewFileSet()
		var parsed []*ast.File
		names := map[*ast.File]string{}
		for _, p := range files {
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			rel, _ := filepath.Rel(root, p)
			names[f] = filepath.ToSlash(rel)
			parsed = append(parsed, f)
		}
		info := typeCheck(fset, parsed)
		for _, f := range parsed {
			for _, site := range execSites(fset, f, info) {
				key := names[f] + "." + site.fn
				if site.git {
					bad = append(bad, names[f]+":"+strconv.Itoa(site.line)+" runs git: "+site.what)
					continue
				}
				if _, ok := execSitesAllowed[key]; ok {
					seen[key] = true
					continue
				}
				bad = append(bad, key+" (line "+strconv.Itoa(site.line)+"): "+site.what)
			}
		}
	}
	for key := range execSitesAllowed {
		if !seen[key] {
			bad = append(bad, "stale execSitesAllowed entry: "+key)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("process starts outside internal/hostgit the guard flags — run git through hostgit, name a constant program, or add a reasoned execSitesAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestExecSitesDetects keeps the guard honest: each shape it must catch is
// caught, and the shapes it must leave alone are left alone.
func TestExecSitesDetects(t *testing.T) {
	type want struct{ git, other int }
	cases := []struct {
		src  string
		want want
	}{
		{`_ = exec.Command("git", "status")`, want{git: 1}},
		{`_ = exec.CommandContext(ctx, "git", "-C", d, "status")`, want{git: 1}},
		{`_ = exec.CommandContext(ctx, "/usr/bin/git", "status")`, want{git: 1}},
		{`_, _ = exec.LookPath("git")`, want{git: 1}},
		{`const bin = "git"; _ = exec.Command(bin, "status")`, want{git: 1}},
		{`_ = exec.Command(gitConst, "status")`, want{git: 1}},
		{`_ = exec.Command("env", "GIT_DIR=x", "git", "status")`, want{git: 1}},
		{`_ = exec.Command("env", "-i", "/usr/bin/git", "status")`, want{git: 1}},
		{`_ = exec.Command("sh", "-c", "git worktree prune")`, want{git: 1}},
		{`_ = exec.CommandContext(ctx, "bash", "-c", "git status")`, want{git: 1}},
		{`_ = exec.Command("dash", "-c", "cd x && git status")`, want{git: 1}},
		{`_ = exec.Command("zsh", "-c", "echo a; git status")`, want{git: 1}},
		{`_ = exec.Command("/bin/sh", "-c", "x=$(git rev-parse HEAD)")`, want{git: 1}},
		{`_ = exec.Command("timeout", "30", "git", "worktree", "prune")`, want{git: 1}},
		{`_ = exec.Command("nice", "-n", "10", "git", "status")`, want{git: 1}},
		{`_ = exec.Command("stdbuf", "-oL", "git", "log")`, want{git: 1}},
		{`_ = exec.Command("ionice", "-c3", "git", "gc")`, want{git: 1}},
		{`_ = exec.Command("setsid", "git", "fetch")`, want{git: 1}},
		{`_ = exec.Command("chrt", "-i", "0", "git", "status")`, want{git: 1}},
		{`_ = exec.Command("timeout", "10", "sh", "-c", "git status")`, want{git: 1}},
		{`_ = exec.Command("timeout", "10", d)`, want{other: 1}},
		{`_ = exec.Command("/usr/libexec/git-core/git-worktree", "prune")`, want{git: 1}},
		{`_ = exec.Command("bash", "-lc", "git status")`, want{git: 1}},
		{`_ = exec.Command("bash", "-ec", "cd x; git status")`, want{git: 1}},
		{`_ = exec.Command("bash", "-lc", d)`, want{other: 1}},
		{`run := exec.CommandContext; _ = run(ctx, "git", "status")`, want{other: 1}},
		{`c := new(exec.Cmd); c.Path = d; _ = c.Run()`, want{other: 1}},
		{`var c exec.Cmd; c.Path = d; _ = c.Run()`, want{other: 1}},
		{`var c *exec.Cmd; _ = c`, want{}},
		{`_ = exec.Command("env", "-S", "git status")`, want{git: 1}},
		{`_ = exec.Command("env", "-Sgit status")`, want{git: 1}},
		{`_ = exec.Command("sh", "-c", "exec \"$0\" status", "git")`, want{git: 1}},
		{`_ = exec.Command("sh", "-c", "exec \"$0\"", d)`, want{other: 1}},
		{`_ = runLoud("git", "status")`, want{git: 1}},
		{`_ = runLoud("podman", "system", "prune")`, want{}},
		{`_ = runLoud(d)`, want{other: 1}},
		{`_ = PodmanExec{}.CommandContext(ctx, "git", "status")`, want{git: 1}},
		{`_ = PodmanExec{}.CommandContext(ctx, "", "info")`, want{}},
		{`_ = exec.Command("podman", "ps")`, want{}},
		{`_ = exec.Command("sh", "-c", "echo digit")`, want{}},
		{`_ = exec.CommandContext(ctx, "systemctl", "show", "git")`, want{}},
		{`_ = exec.Command(d, "git")`, want{other: 1}},
		{`_ = exec.Command("sh", "-c", d)`, want{other: 1}},
		{`_ = exec.Command("bash", args...)`, want{other: 1}},
		{`_ = exec.Command("bash", "script.sh")`, want{other: 1}},
		{`_ = exec.Command("env", d, "status")`, want{other: 1}},
		{`_ = &exec.Cmd{Path: d}`, want{other: 1}},
		{`_, _ = os.StartProcess(d, nil, nil)`, want{other: 1}},
		{`_ = syscall.Exec(d, nil, nil)`, want{other: 1}},
	}
	for _, tc := range cases {
		src := "package x\nimport (\n\"context\"\n\"os\"\n\"os/exec\"\n\"syscall\"\n)\nconst gitConst = \"git\"\n" +
			"var _ = os.Args\nvar _ = syscall.Getpid\nvar _ context.Context\nvar _ *exec.Cmd\n" +
			"func runLoud(name string, args ...string) error { return nil }\n" +
			"type PodmanExec struct{}\nfunc (PodmanExec) CommandContext(ctx context.Context, binary string, args ...string) *exec.Cmd { return nil }\n" +
			"func f(ctx context.Context, d string, args []string) {\n" + tc.src + "\n}\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		info := typeCheck(fset, []*ast.File{f})
		var got want
		for _, s := range execSites(fset, f, info) {
			if s.git {
				got.git++
			} else {
				got.other++
			}
		}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.src, got, tc.want)
		}
	}
}

// TestExecSitesFlagsDotImports: a dot-import of os/exec hides calls behind
// bare names, so the import itself is flagged; a renamed import is followed.
func TestExecSitesFlagsDotImports(t *testing.T) {
	for src, want := range map[string]int{
		"package x\nimport . \"os/exec\"\nfunc f() { _ = Command(\"git\") }\n":       1,
		"package x\nimport run \"os/exec\"\nfunc f() { _ = run.Command(\"git\") }\n": 1,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(execSites(fset, f, typeCheck(fset, []*ast.File{f}))); got != want {
			t.Errorf("%q: %d sites, want %d", src, got, want)
		}
	}
}

// TestGuardSeesThisPackage: pointed at hostgit.go, the guard finds its git
// invocations, so a walker that silently matched nothing could not pass.
func TestGuardSeesThisPackage(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("hostgit.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, "hostgit.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range execSites(fset, f, typeCheck(fset, []*ast.File{f})) {
		if s.git {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("found %d git invocations in hostgit.go, want at least 2 (command and the version probe)", n)
	}
}

// programParams are the allowlisted functions that start whatever program
// their caller names, keyed by function (methods as "Type.Method") with the
// index of that parameter. Their own process start cannot be resolved, so
// every call site's program argument is judged instead, like an exec call's.
var programParams = map[string]int{
	"runLoud":                   0, // internal/admincli/cleanup.go
	"PodmanExec.CommandContext": 1, // internal/sandbox/exec.go ("" means the configured podman)
}

// programParamCallee reports whether call invokes one of programParams,
// resolved through go/types within the calling package (a plain function
// call by name, or a method call whose receiver is the named type).
func programParamCallee(info *types.Info, call *ast.CallExpr) (int, string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if idx, ok := programParams[fun.Name]; ok {
			return idx, fun.Name, true
		}
	case *ast.SelectorExpr:
		sel, ok := info.Selections[fun]
		if !ok || sel.Kind() != types.MethodVal {
			return 0, "", false
		}
		recv := sel.Recv()
		if p, ok := recv.(*types.Pointer); ok {
			recv = p.Elem()
		}
		named, ok := recv.(*types.Named)
		if !ok {
			return 0, "", false
		}
		key := named.Obj().Name() + "." + fun.Sel.Name
		if idx, ok := programParams[key]; ok {
			return idx, key, true
		}
	}
	return 0, "", false
}

// stubImporter satisfies every import with an empty package: the guard only
// needs the constants a package declares itself, so dependencies are not
// type-checked (references into them simply do not resolve to constants).
type stubImporter struct{}

func (stubImporter) Import(p string) (*types.Package, error) {
	pkg := types.NewPackage(p, path.Base(p))
	pkg.MarkComplete()
	return pkg, nil
}

func typeCheck(fset *token.FileSet, files []*ast.File) *types.Info {
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	_, _ = conf.Check("p", fset, files, info)
	return info
}

type execSite struct {
	fn   string
	line int
	git  bool
	what string
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value), true
	}
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s, true
		}
	}
	return "", false
}

// isGit reports git itself or one of its git-* helper programs.
func isGit(s string) bool {
	if s == "" {
		return false
	}
	b := path.Base(s)
	return b == "git" || strings.HasPrefix(b, "git-")
}

var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ash": true}

// shellLineRunsGit reports whether a shell command line runs git anywhere in
// it: any word, after splitting on whitespace and shell punctuation, that is
// git or a path ending in /git.
func shellLineRunsGit(line string) bool {
	words := strings.FieldsFunc(line, func(r rune) bool {
		return strings.ContainsRune(" \t\n;&|()`$<>{}!\"'=", r)
	})
	for _, w := range words {
		if isGit(w) {
			return true
		}
	}
	return false
}

// importNames returns the local names a file gives the packages the guard
// watches.
func importNames(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch p {
		case "os/exec", "os", "syscall", "golang.org/x/sys/unix":
		default:
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

func funcName(fd *ast.FuncDecl) string {
	fn := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		typ := fd.Recv.List[0].Type
		if st, ok := typ.(*ast.StarExpr); ok {
			typ = st.X
		}
		if id, ok := typ.(*ast.Ident); ok {
			fn = id.Name + "." + fn
		}
	}
	return fn
}

func execSites(fset *token.FileSet, f *ast.File, info *types.Info) []execSite {
	imports := importNames(f)
	var out []execSite
	// A dot-import hides every call behind a bare name the guard does not
	// follow: flag the import itself.
	if p, ok := imports["."]; ok {
		out = append(out, execSite{fn: "import", line: fset.Position(f.Package).Line, what: "dot-import of " + p})
	}
	// Function bodies, and package-level values (a `var x = func(...)` seam
	// is code too), each named for the allowlist.
	type unit struct {
		name string
		node ast.Node
	}
	var units []unit
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				units = append(units, unit{funcName(d), d.Body})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range vs.Values {
					name := "var"
					if i < len(vs.Names) {
						name = vs.Names[i].Name
					}
					units = append(units, unit{name, v})
				}
			}
		}
	}
	for _, u := range units {
		fn := u.name
		// Selector nodes already accounted for by their parent: the Fun of a
		// call (judged with its arguments) and the X of a *T (a pointer type
		// constructs nothing).
		handled := map[ast.Node]bool{}
		ast.Inspect(u.node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				handled[n.Fun] = true
			case *ast.StarExpr:
				handled[n.X] = true
			}
			if handled[n] {
				return true
			}
			if site, ok := inspectNode(fset, imports, info, n); ok {
				site.fn = fn
				out = append(out, site)
			}
			return true
		})
	}
	return out
}

// inspectNode reports the process start n is, if it is one the guard flags.
func inspectNode(fset *token.FileSet, imports map[string]string, info *types.Info, n ast.Node) (execSite, bool) {
	at := func(pos token.Pos, git bool, what string) (execSite, bool) {
		return execSite{line: fset.Position(pos).Line, git: git, what: what}, true
	}
	switch n := n.(type) {
	case *ast.SelectorExpr:
		// Not a call's Fun and not under *: an exec.Cmd value (new(exec.Cmd),
		// var c exec.Cmd, exec.Cmd{...}) or exec.Command* used as a value.
		id, ok := n.X.(*ast.Ident)
		if !ok || imports[id.Name] != "os/exec" {
			return execSite{}, false
		}
		switch n.Sel.Name {
		case "Cmd":
			return at(n.Pos(), false, "an exec.Cmd value")
		case "Command", "CommandContext":
			return at(n.Pos(), false, "exec."+n.Sel.Name+" used as a value")
		}
	case *ast.CallExpr:
		if idx, fname, ok := programParamCallee(info, n); ok {
			// A call to a function that takes the program to start as a
			// parameter: judge the program the call site passes.
			if idx < len(n.Args) {
				if git, what, resolved := classify(info, n.Args[idx:], n.Ellipsis.IsValid()); git || !resolved {
					return at(n.Pos(), git, fname+": "+what)
				}
			}
			return execSite{}, false
		}
		sel, ok := n.Fun.(*ast.SelectorExpr)
		if !ok {
			return execSite{}, false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return execSite{}, false
		}
		pkg, name := imports[id.Name], sel.Sel.Name
		switch {
		case pkg == "os" && name == "StartProcess",
			pkg == "syscall" && (name == "Exec" || name == "ForkExec" || name == "StartProcess"),
			pkg == "golang.org/x/sys/unix" && name == "Exec":
			return at(n.Pos(), false, path.Base(pkg)+"."+name)
		case pkg == "os/exec" && name == "LookPath":
			if len(n.Args) == 1 {
				if s, ok := constString(info, n.Args[0]); ok && isGit(s) {
					return at(n.Pos(), true, `exec.LookPath("git")`)
				}
			}
		case pkg == "os/exec" && (name == "Command" || name == "CommandContext"):
			prog := 0
			if name == "CommandContext" {
				prog = 1
			}
			if prog >= len(n.Args) {
				return execSite{}, false
			}
			if git, what, resolved := classify(info, n.Args[prog:], n.Ellipsis.IsValid()); git || !resolved {
				return at(n.Pos(), git, what)
			}
		}
	}
	return execSite{}, false
}

// wrappers run their arguments as a command: the program is somewhere among
// what follows them (after options, durations, priorities, files).
var wrappers = map[string]bool{
	"env": true, "timeout": true, "nice": true, "ionice": true, "stdbuf": true,
	"setsid": true, "chrt": true, "nohup": true, "time": true, "taskset": true,
	"flock": true, "sudo": true, "doas": true, "runuser": true, "unshare": true,
	"nsenter": true, "xargs": true, "systemd-run": true, "busybox": true,
}

// classify decides what an exec call's argv runs: git (directly, as a git-*
// helper, behind a wrapper, or in a shell command line), something the guard
// cannot resolve, or a resolved program that is not git. spread marks a
// final `args...` argument, which the guard cannot read.
func classify(info *types.Info, argv []ast.Expr, spread bool) (git bool, what string, resolved bool) {
	constArg := func(i int) (string, bool) {
		if spread && i == len(argv)-1 {
			return "", false
		}
		return constString(info, argv[i])
	}
	prog, ok := constArg(0)
	if !ok {
		return false, "program is not a constant", false
	}
	if isGit(prog) {
		return true, "program " + strconv.Quote(prog), true
	}
	base := path.Base(prog)
	switch {
	case wrappers[base]:
		// Any later word that is git, or a shell running git, counts; a word
		// the guard cannot read leaves the call unresolved.
		unresolved := false
		for i := 1; i < len(argv); i++ {
			s, ok := constArg(i)
			if !ok {
				unresolved = true
				continue
			}
			if isGit(s) {
				return true, base + " running " + strconv.Quote(s), true
			}
			// A command line passed as one argument (env -S 'git status',
			// env --split-string=…).
			if line := strings.TrimPrefix(strings.TrimPrefix(s, "--split-string="), "-S"); shellLineRunsGit(line) {
				return true, base + " running " + strconv.Quote(s), true
			}
			if shells[path.Base(s)] {
				if g, w, r := classify(info, argv[i:], spread); g || !r {
					return g, base + " running " + w, r
				}
			}
		}
		if unresolved {
			return false, base + " with arguments that are not constants", false
		}
		return false, "", true
	case shells[base]:
		for i := 1; i < len(argv); i++ {
			s, ok := constArg(i)
			if !ok {
				return false, base + " with arguments that are not constants", false
			}
			// -c, or -c combined with other single-letter options (-lc, -ec).
			if !strings.HasPrefix(s, "-") || strings.HasPrefix(s, "--") || !strings.Contains(s, "c") {
				continue
			}
			if i+1 >= len(argv) {
				break
			}
			line, ok := constArg(i + 1)
			if !ok {
				return false, base + " " + s + " with a command line that is not a constant", false
			}
			if shellLineRunsGit(line) {
				return true, base + " " + s + " " + strconv.Quote(line), true
			}
			// What follows the command line becomes $0, $1, …, which it may
			// run (sh -c 'exec "$0"' git).
			for j := i + 2; j < len(argv); j++ {
				p, ok := constArg(j)
				if !ok {
					return false, base + " " + s + " with positional arguments that are not constants", false
				}
				if isGit(p) {
					return true, base + " " + s + " running $" + strconv.Itoa(j-i-2) + " = " + strconv.Quote(p), true
				}
			}
			return false, "", true
		}
		return false, base + " running a script or arguments the guard cannot read", false
	}
	return false, "", true
}
