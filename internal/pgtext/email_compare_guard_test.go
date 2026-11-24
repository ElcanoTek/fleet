package pgtext

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Reasons shared by several caseFoldAllowed entries.
const (
	foldURL       = "a URL scheme or host name, which URLs and DNS compare without regard to case; not an account"
	foldIssuer    = "an OAuth authorization server's issuer URL (issuerMatches documents the rule); not an account"
	foldMethod    = "an HTTP method name against a net/http constant"
	foldConnector = "a connector, server or account-label name from the bundle or a conversation's settings; not an account"
	foldLabel     = "decides only whether a display label repeats the name it labels"
	foldTeam      = "a team label, not an account: a team restated in another case form (Πωλησεις as ΠΩΛΗΣΕΙΣ, where lowercasing gives σ for the final ς) must not move or unshare anyone"
)

// caseFoldAllowed lists the case-insensitive comparisons production code may
// make, keyed "<file>.<Func>: <call>", each with the reason it compares no
// account. A call with a string literal argument (a keyword: "bearer ", "pass",
// "https") needs no entry. Every other strings.EqualFold or bytes.EqualFold,
// every comparison of two strings.ToLower/ToUpper results, and every
// strings.ToLower/ToUpper of an identity-named value (identityName) must be
// listed here: identity emails are compared with SameEmail and keyed with
// EmailKey.
var caseFoldAllowed = map[string]string{
	"cmd/fleet/chat_tasks.go.resolveManagedTasks: strings.EqualFold(strings.TrimSpace(*t.Model), model)":                                "a model slug",
	"internal/admincli/mcp.go.mcpAccountDel: strings.EqualFold(baseVar, *keyVar)":                                                       "an environment-variable name an operator typed",
	"internal/agentcore/approval_resume_policy.go.buildResumeSet: strings.EqualFold(strings.TrimSpace(v), ApprovalModeNotify)":          "an approval-mode keyword against its constant",
	"internal/agentcore/autonomous_floor.go.extractTopLevelStringFieldSpellings: strings.EqualFold(strings.TrimSpace(key), wantKey)":    "a JSON field name in a tool's arguments, read in every spelling encoding/json accepts",
	"internal/agentcore/openrouter_models.go.hasModality: strings.EqualFold(strings.TrimSpace(m), want)":                                "a model modality keyword",
	"internal/agentcore/provider_probe.go.sameOriginRedirect: strings.EqualFold(req.URL.Host, first.Host)":                              foldURL,
	"internal/agentcore/provider_probe.go.sameOriginRedirect: strings.EqualFold(req.URL.Scheme, first.Scheme)":                          foldURL,
	"internal/agentcore/provider_stream_error.go.contextLengthRejection: strings.EqualFold(detail.Code, contextLengthCode)":             "a provider error code against its constant",
	"internal/agentcore/provider_stream_error.go.contextLengthRejection: strings.EqualFold(detail.ProviderCode, contextLengthCode)":     "a provider error code against its constant",
	"internal/agent/prompt.go.writeAvailableOptionalConnectorsSection: strings.EqualFold(label, srv.Name)":                              foldLabel,
	"internal/agent/prompt.go.writeNeedsSetupConnectorsSection: strings.EqualFold(label, c.Name)":                                       foldLabel,
	"internal/agent/session.go.FormatModelUpgradeNotice: strings.EqualFold(from, to)":                                                   "two model slugs, for the wording of a notice",
	"internal/httpapi/approval_browser_summary.go.withBrowserSessionSite: strings.EqualFold(now, start)":                                foldURL,
	"internal/httpapi/connector_studio.go.checkCustomToolTestReadOnly: strings.EqualFold(c.Name, wantName)":                             foldConnector,
	"internal/httpapi/connector_studio.go.checkCustomToolTestReadOnly: strings.EqualFold(ct.Method, http.MethodGet)":                    foldMethod,
	"internal/httpapi/connector_studio.go.enableConnectorOnConversation: strings.EqualFold(strings.TrimSpace(existing), norm)":          foldConnector,
	"internal/httpapi/connector_studio.go.isCatalogApprovedHost: strings.EqualFold(strings.TrimSpace(entry.Name), connectorName)":       foldConnector,
	"internal/httpapi/connector_studio.go.resolveMountedCustomTool: strings.EqualFold(seat.Account, wantAcct)":                          foldConnector,
	"internal/httpapi/connector_studio.go.resolveMountedCustomTool: strings.EqualFold(seat.Server, wantName)":                           foldConnector,
	"internal/httpapi/enable_mcp_connectors.go.summarizeEnableMCPConnectorsInput: strings.EqualFold(dn, c)":                             foldLabel,
	"internal/httpapi/session_approvals.go.patternValues: strings.EqualFold(a.name, argName)":                                           "a tool argument's JSON field name, read in every spelling encoding/json accepts",
	"internal/mcpoauth/discovery.go.Discover: strings.EqualFold(strings.TrimRight(issuer, \"/\"), strings.TrimRight(as.Issuer, \"/\"))": foldIssuer,
	"internal/mcpoauth/discovery.go.issuerMatches: strings.EqualFold(as[i], entraTenantTemplate)":                                       foldIssuer,
	"internal/mcpoauth/discovery.go.issuerMatches: strings.EqualFold(e, a)":                                                             foldIssuer,
	"internal/mcpoauth/discovery.go.issuerMatches: strings.EqualFold(es[i], as[i])":                                                     foldIssuer,
	"internal/mcpoauth/discovery.go.issuerMatches: strings.EqualFold(eu.Host, au.Host)":                                                 foldURL,
	"internal/mcpoauth/discovery.go.upgradeAdvertisedPointer: strings.EqualFold(ptr.Hostname(), su.Hostname())":                         foldURL,
	"internal/mcpoauth/register.go.confidentialMethodFor: strings.EqualFold(strings.TrimSpace(m), want)":                                "an OAuth client authentication method keyword",
	"internal/mcp/redirect.go.stripHeadersOnCrossOriginRedirect: strings.EqualFold(req.URL.Host, origin.URL.Host)":                      foldURL,
	"internal/mcp/redirect.go.stripHeadersOnCrossOriginRedirect: strings.EqualFold(req.URL.Scheme, origin.URL.Scheme)":                  foldURL,
	"internal/ms365mcp/mcp.go.isMutatingGraphTool: strings.EqualFold(s.Method, http.MethodGet)":                                         foldMethod,
	"internal/ms365mcp/mcp.go.isMutatingGraphTool: strings.EqualFold(s.Method, http.MethodPost)":                                        foldMethod,
	"internal/ms365mcp/oauth.go.addToolScopes: strings.EqualFold(t.Method, http.MethodGet)":                                             foldMethod,
	"internal/ms365mcp/oauth.go.isRedirectURIAllowed: strings.EqualFold(parsed.Hostname(), pp.Hostname())":                              foldURL,
	"internal/ms365mcp/oauth.go.isRedirectURIAllowed: strings.EqualFold(parsed.Scheme, pp.Scheme)":                                      foldURL,
	"internal/playwrightrunner/runner.go.sameOriginURL: strings.EqualFold(ua.Host, ub.Host)":                                            foldURL,
	"internal/playwrightrunner/runner.go.sameOriginURL: strings.EqualFold(ua.Scheme, ub.Scheme)":                                        foldURL,
	"internal/release/fetch.go.verifiedManifest: strings.EqualFold(m.Commit, commit)":                                                   "a hexadecimal commit id",
	"internal/remotemcp/service.go.inheritExistingCustomAuth: strings.EqualFold(ex.Name, name)":                                         foldConnector,
	"internal/remotemcp/service.go.sameCustomOrigin: strings.EqualFold(ua.Host, ub.Host)":                                               foldURL,
	"internal/remotemcp/service.go.sameCustomOrigin: strings.EqualFold(ua.Scheme, ub.Scheme)":                                           foldURL,
	"internal/remotemcp/service.go.upsertCustomConnector: strings.EqualFold(ex.Name, name)":                                             foldConnector,
	"internal/sched/handlers/middleware.go.originMatchesHost: strings.EqualFold(u.Host, expected)":                                      foldURL,
	"internal/store/access_provisioning.go.applyExternalTeamTx: strings.EqualFold(strings.TrimSpace(cur.String), team)":                 foldTeam,
	"internal/store/users.go.SetOwnTeam: strings.EqualFold(cur.TeamID, team)":                                                           foldTeam,
	"internal/store/users.go.unshareOnTeamChangeTx: strings.EqualFold(old, newTeam)":                                                    foldTeam,
	"internal/tools/fs.go.runEditFile: strings.EqualFold(strings.TrimSpace(params.ExpectedHash), sandbox.ExpectAbsent)":                 "a file hash or its \"absent\" keyword",
	"internal/webpush/webpush.go.hasPrefixFold: strings.EqualFold(s[:len(prefix)], prefix)":                                             "a URI scheme prefix (mailto:, https:)",
}

// TestIdentityEmailsAreNotCaseFolded keeps the account comparison in one
// place. strings.EqualFold applies Unicode case folding, which calls two
// different lowercase addresses the same (σ@x.com and ς@x.com, µ@x.com and
// μ@x.com); each of them can be its own account, so an owner, sender or
// viewer check that folds case would treat one account as the other. Such
// checks go through SameEmail, which compares the keys the store keeps
// accounts under.
//
// The guard fails on any strings.EqualFold or bytes.EqualFold call (or the
// function used as a value) in a non-test file under cmd/ or internal/, on
// any == or != between two strings.ToLower/ToUpper results, and on any
// strings.ToLower/ToUpper of a value named like an identity email
// (identityName: an email, owner, sender, viewer, user, …), which keys an
// account another way than EmailKey — unless one argument is a string
// literal or the call is a reviewed caseFoldAllowed entry. The name test is
// a heuristic: a value called r or p is not seen, so it catches the usual
// spellings, not every one.
func TestIdentityEmailsAreNotCaseFolded(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var bad []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, site := range caseFoldSites(t, path) {
				key := rel + "." + site.fn + ": " + site.src
				if _, ok := caseFoldAllowed[key]; ok {
					seen[key] = true
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(site.line)+" "+site.fn+": "+site.src)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range caseFoldAllowed {
		if !seen[key] {
			bad = append(bad, "stale caseFoldAllowed entry: "+key)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("case-insensitive comparison outside the reviewed list — compare identity emails with pgtext.SameEmail, or add a caseFoldAllowed entry saying why the values are not accounts:\n  %s", strings.Join(bad, "\n  "))
	}
}

// identityWords are the words (of a camelCase or snake_case name, singular)
// that mark a value as an identity email; identityWholeNames are names that do
// so only on their own ("user" is one, "userMessage" is not).
var (
	identityWords      = map[string]bool{"email": true, "owner": true, "sender": true, "viewer": true, "caller": true, "principal": true, "participant": true, "grantee": true, "creator": true, "actor": true, "username": true}
	identityWholeNames = map[string]bool{"user": true, "userfromctx": true}
)

// identityName reports whether name, the last identifier of the expression
// strings.ToLower/ToUpper is called on (through strings.TrimSpace:
// userEmail, conv.UserEmail, entry.steer.user, userFromCtx(ctx)), names an
// identity email. It matches whole words only, so factor or ownership do not
// count; the name with its underscores dropped is also tried whole, so
// userName, user_name and an all-lowercase useremail count too.
func identityName(name string) bool {
	joined := strings.TrimSuffix(strings.ToLower(strings.ReplaceAll(name, "_", "")), "s")
	if identityWholeNames[joined] || identityWords[joined] || strings.HasSuffix(joined, "email") {
		return true
	}
	for _, w := range nameWords(name) {
		if identityWords[strings.TrimSuffix(w, "s")] {
			return true
		}
	}
	return false
}

// nameWords splits an identifier at underscores, digits and camelCase
// boundaries (userEmail → user, email; URLOwner → url, owner), lowercased.
func nameWords(name string) []string {
	var out []string
	rs := []rune(name)
	start := 0
	flush := func(end int) {
		if end > start {
			out = append(out, strings.ToLower(string(rs[start:end])))
		}
	}
	for i, r := range rs {
		if r == '_' || unicode.IsDigit(r) {
			flush(i)
			start = i + 1
			continue
		}
		if i > start && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
			flush(i)
			start = i
		}
	}
	flush(len(rs))
	return out
}

type caseFoldSite struct {
	fn   string
	src  string
	line int
}

// caseFoldSites returns the case-insensitive comparisons in the file at path
// that caseFoldAllowed must cover: each with its enclosing function (or
// "<decl>" outside one) and its source text on one line.
func caseFoldSites(t *testing.T, path string) []caseFoldSite {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	text := func(n ast.Node) string {
		return strings.Join(strings.Fields(string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])), " ")
	}
	var out []caseFoldSite
	for _, decl := range f.Decls {
		fn := "<decl>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		called := map[*ast.SelectorExpr]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				se, ok := n.Fun.(*ast.SelectorExpr)
				if ok && (isPkgFunc(se, "ToLower", "strings", "bytes") || isPkgFunc(se, "ToUpper", "strings", "bytes")) && len(n.Args) == 1 && identityName(lastName(n.Args[0])) {
					out = append(out, caseFoldSite{fn: fn, src: text(n), line: fset.Position(n.Pos()).Line})
					return true
				}
				if !ok || !isPkgFunc(se, "EqualFold", "strings", "bytes") {
					return true
				}
				called[se] = true
				for _, a := range n.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						return true
					}
				}
				out = append(out, caseFoldSite{fn: fn, src: text(n), line: fset.Position(n.Pos()).Line})
			case *ast.SelectorExpr:
				// EqualFold passed as a value (slices.EqualFunc(a, b,
				// strings.EqualFold)). A call's own selector was seen first.
				if isPkgFunc(n, "EqualFold", "strings", "bytes") && !called[n] {
					out = append(out, caseFoldSite{fn: fn, src: text(n), line: fset.Position(n.Pos()).Line})
				}
			case *ast.BinaryExpr:
				if (n.Op == token.EQL || n.Op == token.NEQ) && isCaseMapCall(n.X) && isCaseMapCall(n.Y) {
					out = append(out, caseFoldSite{fn: fn, src: text(n), line: fset.Position(n.Pos()).Line})
				}
			}
			return true
		})
	}
	return out
}

// isPkgFunc reports whether se names function name of one of pkgs.
func isPkgFunc(se *ast.SelectorExpr, name string, pkgs ...string) bool {
	id, ok := se.X.(*ast.Ident)
	if !ok || se.Sel.Name != name {
		return false
	}
	for _, p := range pkgs {
		if id.Name == p {
			return true
		}
	}
	return false
}

// isCaseMapCall reports whether e is a strings/bytes ToLower or ToUpper call.
func isCaseMapCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	se, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return isPkgFunc(se, "ToLower", "strings", "bytes") || isPkgFunc(se, "ToUpper", "strings", "bytes")
}

// lastName is the last identifier of e, looking through strings.TrimSpace, a
// call's function, an index, a dereference and parentheses: the name that
// says what e holds.
func lastName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.CallExpr:
		if se, ok := x.Fun.(*ast.SelectorExpr); ok && isPkgFunc(se, "TrimSpace", "strings", "bytes") && len(x.Args) == 1 {
			return lastName(x.Args[0])
		}
		return lastName(x.Fun)
	case *ast.IndexExpr:
		return lastName(x.X)
	case *ast.StarExpr:
		return lastName(x.X)
	case *ast.ParenExpr:
		return lastName(x.X)
	}
	return ""
}

func TestIdentityNameMatchesWholeWords(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"userEmail": true, "UserEmail": true, "uEmail": true, "ownerEmail": true,
		"subViewers": true, "user": true, "userFromCtx": true, "username": true,
		"URLOwner": true, "sender_email": true, "email": true,
		"factor": false, "ownership": false, "userMessage": false, "users2go": false,
		"emailer": false, "contractor": false, "name": false,
		"userName": true, "UserName": true, "user_name": true, "useremail": true,
		"owneremails": true, "users": true, "sendemails": true, "contractor_id": false,
	} {
		if got := identityName(name); got != want {
			t.Errorf("identityName(%q) = %v, want %v (words %v)", name, got, want, nameWords(name))
		}
	}
}
