package main

// The auth half of the OpenAPI contract: every operation's declared
// `x-fleet-permission`, the top-level `x-fleet-api-version`, the `/v1` server
// prefix and the `x-fleet-key-types` table — the facts an external consumer
// (manifest's contract check, docs/MANIFEST-CONTRACT.md) reads from the spec
// instead of grepping handler source. openapi_drift_test.go proves the
// {method, path} set and the security schemes; this file proves the
// permission each operation declares is the one the code enforces.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// Non-permission values of x-fleet-permission (see the spec's info.description).
const (
	permNone          = "none"          // takes no API key; security: []
	permAuthenticated = "authenticated" // any valid key; checked per JSON-RPC method (A2A)
)

// specOp is one operation as the auth-contract tests see it.
type specOp struct {
	method, path string
	permission   string   // x-fleet-permission ("" when absent)
	security     []string // scheme names; empty for security: []
	hasSecurity  bool     // the operation declares a security key at all
}

func (o specOp) String() string { return o.method + " " + o.path }

// loadRawOpenAPI parses docs/openapi.yaml generically, so vendor extensions
// (which openAPIDoc deliberately does not model) are reachable.
func loadRawOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func specOps(t *testing.T) []specOp {
	t.Helper()
	paths, _ := loadRawOpenAPI(t)["paths"].(map[string]any)
	var ops []specOp
	for path, rawItem := range paths {
		item, _ := rawItem.(map[string]any)
		for key, rawOp := range item {
			if !httpMethods[strings.ToUpper(key)] {
				continue
			}
			op, _ := rawOp.(map[string]any)
			perm, _ := op["x-fleet-permission"].(string)
			_, hasSec := op["security"]
			ops = append(ops, specOp{
				method:      strings.ToUpper(key),
				path:        path,
				permission:  perm,
				security:    securitySchemeNames(op),
				hasSecurity: hasSec,
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
	return ops
}

// TestOpenAPIPermissionDeclared is the static half: every operation declares
// exactly one x-fleet-permission from the vocabulary, consistent with its
// security list. It runs without a database.
func TestOpenAPIPermissionDeclared(t *testing.T) {
	ops := specOps(t)
	if len(ops) == 0 {
		t.Fatal("no operations parsed from docs/openapi.yaml")
	}
	for _, op := range ops {
		p := op.permission
		switch {
		case p == "":
			t.Errorf("%s: missing x-fleet-permission", op)
			continue
		case p == permNone, p == permAuthenticated:
		case models.Permission(p).Valid():
		default:
			t.Errorf("%s: x-fleet-permission %q is neither a models.Permission nor %q/%q", op, p, permNone, permAuthenticated)
			continue
		}
		if !op.hasSecurity {
			t.Errorf("%s: no security key (declare security: [] for a public operation)", op)
		}
		public := op.hasSecurity && len(op.security) == 0
		if public != (p == permNone) {
			t.Errorf("%s: x-fleet-permission %q but security %v — %q and security: [] go together", op, p, op.security, permNone)
		}
		if p != permNone && p != string(models.PermissionAdmin) && !slices.Contains(op.security, "scopedApiKey") {
			t.Errorf("%s: x-fleet-permission %q but security does not list scopedApiKey — only admin-gated operations omit it", op, p)
		}
	}
}

// TestOpenAPIAPIVersion pins the version facts: x-fleet-api-version equals
// apiversion.Version, and servers[0].url is the prefix the apiversion.Router
// actually serves (a request under it reaches the inner route and is stamped
// with that version).
func TestOpenAPIAPIVersion(t *testing.T) {
	doc := loadRawOpenAPI(t)
	if got, _ := doc["x-fleet-api-version"].(string); got != apiversion.Version {
		t.Errorf("x-fleet-api-version = %q, want apiversion.Version %q", got, apiversion.Version)
	}

	prefix := specServerPrefix(t)
	if prefix != "/v1" {
		t.Errorf("servers[0].url = %q, want /v1", prefix)
	}
	h := handlers.New(handlers.Config{}, nil, nil)
	router := apiversion.Router(buildOrchestratorMux(h, handlers.NewNotesHandlers(nil, h), reloadConfigHandler(nil), mcpReloadHandler(nil), nil))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, prefix+"/api-info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s/api-info without credentials = %d, want 200 (public, security: [])", prefix, w.Code)
	}
	if got := w.Header().Get(apiversion.VersionHeader); got != apiversion.Version {
		t.Errorf("%s = %q under servers[0].url, want %q", apiversion.VersionHeader, got, apiversion.Version)
	}
}

// TestOpenAPIKeyTypes pins x-fleet-key-types to internal/sched/apikeys: each
// type's permission set (KeyType.Permissions) and the methods it may use on
// the scoped-key route group (TypeAllowsMethod).
func TestOpenAPIKeyTypes(t *testing.T) {
	raw, _ := loadRawOpenAPI(t)["x-fleet-key-types"].(map[string]any)
	types := []apikeys.KeyType{apikeys.KeyTypeAdmin, apikeys.KeyTypeTask, apikeys.KeyTypeReadonly, apikeys.KeyTypeWebhook}
	if len(raw) != len(types) {
		t.Errorf("x-fleet-key-types has %d entries, want %d (%v)", len(raw), len(types), types)
	}
	allMethods := []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}
	for _, kt := range types {
		entry, ok := raw[string(kt)].(map[string]any)
		if !ok {
			t.Errorf("x-fleet-key-types.%s missing", kt)
			continue
		}
		var gotPerms []string
		for _, p := range asSlice(entry["permissions"]) {
			s, _ := p.(string)
			gotPerms = append(gotPerms, s)
		}
		var wantPerms []string
		for _, p := range kt.Permissions() {
			wantPerms = append(wantPerms, string(p))
		}
		sort.Strings(gotPerms)
		sort.Strings(wantPerms)
		if !slices.Equal(gotPerms, wantPerms) {
			t.Errorf("x-fleet-key-types.%s.permissions = %v, want %v", kt, gotPerms, wantPerms)
		}

		var specAllows func(string) bool
		if s, ok := entry["methods"].(string); ok && s == "any" {
			specAllows = func(string) bool { return true }
		} else {
			var listed []string
			for _, m := range asSlice(entry["methods"]) {
				s, _ := m.(string)
				listed = append(listed, s)
			}
			specAllows = func(m string) bool { return slices.Contains(listed, m) }
		}
		for _, m := range allMethods {
			if got, want := specAllows(m), apikeys.TypeAllowsMethod(kt, m); got != want {
				t.Errorf("x-fleet-key-types.%s.methods admits %s = %v, TypeAllowsMethod = %v", kt, m, got, want)
			}
		}
	}
}

// specAPIKeyHeader returns the header the API-key security schemes name. The
// admin and scoped schemes must agree: fleet reads one header for both.
func specAPIKeyHeader(t *testing.T) string {
	t.Helper()
	comps, _ := loadRawOpenAPI(t)["components"].(map[string]any)
	schemes, _ := comps["securitySchemes"].(map[string]any)
	var name string
	for _, scheme := range []string{"adminApiKey", "scopedApiKey"} {
		def, _ := schemes[scheme].(map[string]any)
		if in, _ := def["in"].(string); in != "header" {
			t.Fatalf("securitySchemes.%s.in = %q, want header", scheme, in)
		}
		n, _ := def["name"].(string)
		if n == "" || (name != "" && n != name) {
			t.Fatalf("securitySchemes.%s.name = %q (other API-key scheme: %q)", scheme, n, name)
		}
		name = n
	}
	return name
}

// specServerPrefix returns servers[0].url, the versioned base.
func specServerPrefix(t *testing.T) string {
	t.Helper()
	servers, _ := loadRawOpenAPI(t)["servers"].([]any)
	if len(servers) == 0 {
		t.Fatal("spec has no servers")
	}
	first, _ := servers[0].(map[string]any)
	url, _ := first["url"].(string)
	return strings.TrimSuffix(url, "/")
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// probeExcluded lists operations whose declared permission the enforcement
// probe cannot exercise, each with the reason. Every other operation with a
// permission value is probed.
var probeExcluded = map[string]string{
	// A2A (#1279) answers 501 before any auth unless FLEET_A2A_ENABLED wires
	// SetA2A, and when wired it reports per-method refusals as JSON-RPC errors
	// in a 200 — so HTTP status can prove nothing about the key. Its
	// per-method permissions are covered by internal/sched/handlers' a2a tests.
	"POST /a2a":  "A2A: 501 until wired; per-method permission refusals are JSON-RPC errors in a 200",
	"POST /a2a/": "A2A: 501 until wired; per-method permission refusals are JSON-RPC errors in a 200",
}

// probeKey is one credential the enforcement probe sends.
type probeKey struct {
	name  string
	raw   string
	keyID *string // the task-attribution id (nil for the bootstrap admin key)
}

// TestOpenAPIPermissionEnforcement is the behavioural half: for every
// operation that declares a permission, it drives real requests through the
// real router (apiversion.Router over buildOrchestratorMux, real Handlers on
// a real sched DB, a real key manager) and asserts the declared value is what
// the code enforces:
//
//   - the bootstrap ADMIN_API_KEY is admitted everywhere;
//   - each typed key is admitted exactly when its type holds the permission
//     (admin implies all) and — for readonly — the method is safe, and is
//     otherwise a definitive 403;
//   - a legacy sk- key holding exactly {permission, view_tasks} is admitted
//     (view_tasks is the scoped-group baseline for legacy keys; skipped for
//     admin, which legacy keys never reach on AdminAuthMiddleware routes);
//   - a legacy key holding every permission EXCEPT the declared one (and
//     admin) is refused with 401 or 403.
//
// "Admitted" means the response is neither 401 nor 403: a 400/404/409 from a
// deliberately minimal request proves the request got past authorization,
// which is all this test claims. Every {task_id} is a fresh task created by
// the acting key (so own-rows rules never mask the permission gate), in a
// terminal status so the stream route replays instead of following.
func TestOpenAPIPermissionEnforcement(t *testing.T) {
	database := testdb.SchedDB(t, "logs", "tasks", "users")
	store := storage.New()
	store.SetDatabase(database)

	tmp := t.TempDir()
	keyMgr, err := apikeys.NewManager(filepath.Join(tmp, "api_keys.json"), filepath.Join(tmp, "audit.jsonl"))
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	const adminKey = "openapi-permissions-admin-key"
	h := handlers.New(handlers.Config{
		AdminAPIKey:      adminKey,
		DefaultTaskModel: "test/model",
		Version:          "0.0.0-test",
		DataDir:          tmp,
	}, store, keyMgr)
	notes := handlers.NewNotesHandlers(sched.NewStore(database), h)
	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	router := apiversion.Router(buildOrchestratorMux(h, notes, noop, noop, nil))

	typed := map[apikeys.KeyType]probeKey{}
	for _, kt := range []apikeys.KeyType{apikeys.KeyTypeAdmin, apikeys.KeyTypeTask, apikeys.KeyTypeReadonly, apikeys.KeyTypeWebhook} {
		key, raw, err := keyMgr.CreateTypedKey("probe-"+string(kt), kt, []string{"probe"}, 0, nil, "")
		if err != nil {
			t.Fatalf("create %s key: %v", kt, err)
		}
		id := key.KeyID
		typed[kt] = probeKey{name: "typed " + string(kt) + " key", raw: raw, keyID: &id}
	}
	legacy := func(perms ...models.Permission) probeKey {
		key, raw, err := keyMgr.CreateKey("probe-legacy-"+uuid.NewString(), perms, nil, 0, nil, "")
		if err != nil {
			t.Fatalf("create legacy key %v: %v", perms, err)
		}
		if !slices.Equal(key.Permissions, perms) {
			t.Fatalf("legacy key minted with %v, asked for %v", key.Permissions, perms)
		}
		id := key.KeyID
		return probeKey{name: "legacy key " + permList(perms), raw: raw, keyID: &id}
	}
	bootstrap := probeKey{name: "bootstrap ADMIN_API_KEY", raw: adminKey}

	// The credential header and the base path come from the spec, so a wrong
	// securitySchemes name or servers[0].url fails every admission below.
	header, prefix := specAPIKeyHeader(t), specServerPrefix(t)

	send := func(t *testing.T, op specOp, k probeKey) int {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "openapi permission probe", Status: models.TaskStatusSuccess,
			CreatedAt: time.Now().UTC(), CreatedByKeyID: k.keyID}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		path := prefix + fillPath(op.path, task.ID.String())
		var body *strings.Reader
		if op.method == http.MethodPost || op.method == http.MethodPut {
			body = strings.NewReader("{}")
		} else {
			body = strings.NewReader("")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(op.method, path, body).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(header, k.raw)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	admitted := func(code int) bool { return code != http.StatusUnauthorized && code != http.StatusForbidden }

	probed := map[string]bool{}
	for _, op := range specOps(t) {
		if op.permission == "" || op.permission == permNone || op.permission == permAuthenticated {
			continue
		}
		if _, skip := probeExcluded[op.String()]; skip {
			continue
		}
		probed[op.String()] = true
		perm := models.Permission(op.permission)
		t.Run(op.String(), func(t *testing.T) {
			if code := send(t, op, bootstrap); !admitted(code) {
				t.Errorf("%s: %d, want admitted", bootstrap.name, code)
			}

			for kt, k := range typed {
				want := typeHolds(kt, perm) && apikeys.TypeAllowsMethod(kt, op.method)
				// The admin-only routes and the create family sit outside the
				// type/method lattice, but the lattice agrees with them: only
				// a type holding the permission passes either way.
				if perm == models.PermissionAdmin {
					want = kt == apikeys.KeyTypeAdmin
				}
				wantRefusal := http.StatusForbidden
				if op.String() == "GET /files/{filename}" {
					// HandleDownload accepts the bootstrap key only.
					want, wantRefusal = false, http.StatusUnauthorized
				}
				code := send(t, op, k)
				switch {
				case want && !admitted(code):
					t.Errorf("%s holds %s but got %d, want admitted", k.name, perm, code)
				case !want && code != wantRefusal:
					t.Errorf("%s lacks %s (or its type is method-limited) but got %d, want %d", k.name, perm, code, wantRefusal)
				}
			}

			if perm != models.PermissionAdmin {
				holder := legacy(uniquePerms(perm, models.PermissionViewTasks)...)
				if code := send(t, op, holder); !admitted(code) {
					t.Errorf("%s: %d, want admitted", holder.name, code)
				}
			}
			var rest []models.Permission
			for _, p := range models.AllPermissions {
				if p != perm && p != models.PermissionAdmin {
					rest = append(rest, p)
				}
			}
			lacking := legacy(rest...)
			if code := send(t, op, lacking); admitted(code) {
				t.Errorf("%s (everything but %s): %d, want 401/403", lacking.name, perm, code)
			}
		})
	}

	// The probe must reach every operation the manifest contract reads, so a
	// future exclusion cannot quietly drop one (docs/MANIFEST-CONTRACT.md).
	for _, op := range []string{
		"POST /tasks", "POST /tasks/estimate", "POST /upload",
		"GET /tasks/{task_id}", "GET /tasks/{task_id}/output", "GET /logs/{task_id}",
		"GET /mcp-accounts", "GET /mcp-servers",
	} {
		if !probed[op] {
			t.Errorf("%s is read by the manifest contract but was not probed", op)
		}
	}
}

// typeHolds reports whether a typed key of this type carries perm (admin
// implies every permission, as APIKey.HasPermission does).
func typeHolds(kt apikeys.KeyType, perm models.Permission) bool {
	for _, p := range kt.Permissions() {
		if p == models.PermissionAdmin || p == perm {
			return true
		}
	}
	return false
}

func uniquePerms(perms ...models.Permission) []models.Permission {
	var out []models.Permission
	for _, p := range perms {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func permList(perms []models.Permission) string {
	s := make([]string, len(perms))
	for i, p := range perms {
		s[i] = string(p)
	}
	return "{" + strings.Join(s, ",") + "}"
}

// fillPath substitutes the spec's path parameters: {task_id} with the probe's
// fresh task, the rest with values that name nothing (so a request that clears
// authorization ends in a 400/404, never a side effect on a real row).
func fillPath(path, taskID string) string {
	out := strings.ReplaceAll(path, "{task_id}", taskID)
	out = strings.ReplaceAll(out, "{version}", "1")
	out = strings.ReplaceAll(out, "{filename}", "no-such-file.txt")
	out = strings.ReplaceAll(out, "{slug}", "no-such-slug")
	out = strings.ReplaceAll(out, "/*", "/no-such-file.txt")
	for strings.Contains(out, "{") {
		i := strings.Index(out, "{")
		j := strings.Index(out[i:], "}")
		if j < 0 {
			break
		}
		out = out[:i] + uuid.NewString() + out[i+j+1:]
	}
	return out
}
