package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// roleHolds reports whether a user role's permission set (the same
// models.RolePermissions table userHasPermission reads) grants perm; admin
// implies every permission.
func roleHolds(role string, perm models.Permission) bool {
	for _, p := range models.RolePermissions[role] {
		if p == models.PermissionAdmin || p == perm {
			return true
		}
	}
	return false
}

// ownerMayAct lists the cancel_task operations a task's creating user may
// perform without the permission (taskResumableByPrincipal: the creator may
// answer or wake their own task).
var ownerMayAct = map[string]bool{
	"POST /tasks/{task_id}/resume": true,
	"POST /tasks/{task_id}/wake":   true,
}

// TestOrchestratorUserRoleCannotMutateWhenReadonly drives every operation a
// user principal can reach (security lists elcanoAuthCookie) through the real
// router, as an account of each Operations Center role on the user path — the
// web tier's header-trust session (shared token + X-User-Email; the password
// bearer is retired, #2049 M4) — and pins the role gate:
//
//   - readonly on any mutating method (POST/PUT/PATCH/DELETE) is a definitive
//     403, the same line a fleet_readonly_ key draws (apikeys.TypeAllowsMethod);
//     the deployed bug was POST /tasks answering 200 to a readonly bearer;
//   - readonly on a safe method keeps every read its role grants (admitted
//     exactly when models.RolePermissions holds the operation's permission);
//   - client and admin keep exactly the access they had: admitted when the
//     role holds the operation's permission, or on an ownerMayAct operation
//     (resume/wake a task the user created), and 403 otherwise.
//
// "Admitted" is the probe's meaning: neither 401 nor 403. Every {task_id} is a
// fresh terminal task created by the acting user, so own-rows rules never mask
// the role gate.
func TestOrchestratorUserRoleCannotMutateWhenReadonly(t *testing.T) {
	t.Parallel()
	env := newProbeEnv(t)
	prefix := specServerPrefix(t)

	type actor struct {
		user *models.User
	}
	actors := map[string]actor{}
	for _, role := range []string{"readonly", "client", "admin"} {
		// One identity table (#2049 M4): the legacy role is what the
		// account's Fleet role and ops_scope grant.
		acct := identity.NewAccount{Email: role + "-user@probe.example", Role: identity.RoleViewer}
		switch role {
		case "admin":
			acct.Role = identity.RoleAdmin
		case "client":
			acct.OpsScope = identity.OpsRun
		case "readonly":
			acct.OpsScope = identity.OpsView
		}
		created, err := env.store.Identity().CreateAccount(context.Background(), acct)
		if err != nil {
			t.Fatalf("add %s user: %v", role, err)
		}
		actors[role] = actor{user: &models.User{ID: created.ID, Username: created.Email, Role: role}}
	}

	credentials := []struct {
		name  string
		apply func(r *http.Request, a actor)
	}{
		{"session", func(r *http.Request, a actor) {
			r.Header.Set("X-Orchestrator-Server-Token", env.sharedToken)
			r.Header.Set("X-User-Email", a.user.Username)
		}},
	}

	send := func(t *testing.T, op specOp, a actor, apply func(*http.Request, actor)) (int, string) {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "readonly role probe", Status: models.TaskStatusSuccess,
			CreatedAt: time.Now().UTC(), CreatedBy: &a.user.ID}
		if _, err := env.store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		body := ""
		if op.method == http.MethodPost || op.method == http.MethodPut || op.method == http.MethodPatch {
			body = "{}"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(op.method, prefix+fillPath(op.path, task.ID.String()), strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		if op.method != http.MethodGet && op.method != http.MethodHead {
			req.Header.Set("Origin", "http://"+req.Host) // what a browser sends; CSRF, not auth
		}
		apply(req, a)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	admitted := func(code int) bool { return code != http.StatusUnauthorized && code != http.StatusForbidden }

	var mutating, reads int
	for _, op := range specOps(t) {
		if !slices.Contains(op.security, "elcanoAuthCookie") {
			continue // not reachable by a user principal
		}
		perm := models.Permission(op.permission)
		safe := apikeys.TypeAllowsMethod(apikeys.KeyTypeReadonly, op.method)
		if safe {
			reads++
		} else {
			mutating++
		}
		t.Run(op.String(), func(t *testing.T) {
			for _, cred := range credentials {
				for _, role := range []string{"readonly", "client", "admin"} {
					want := roleHolds(role, perm) || (ownerMayAct[op.String()] && role != "readonly")
					if role == "readonly" && !safe {
						want = false
					}
					code, body := send(t, op, actors[role], cred.apply)
					switch {
					case want && !admitted(code):
						t.Errorf("%s %s user: %d (%s), want admitted", role, cred.name, code, body)
					case !want && code != http.StatusForbidden:
						t.Errorf("%s %s user: %d (%s), want 403", role, cred.name, code, body)
					}
				}
			}
		})
	}
	if mutating == 0 || reads == 0 {
		t.Fatalf("probed %d mutating and %d read operations; the spec should give both", mutating, reads)
	}

	// The deployed regression, literally: a valid create body. readonly gets
	// 403 and no task; client and admin still get 200.
	for _, cred := range credentials {
		for _, role := range []string{"readonly", "client", "admin"} {
			req := httptest.NewRequest(http.MethodPost, prefix+"/tasks", strings.NewReader(`{"prompt":"readonly create probe"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://"+req.Host)
			cred.apply(req, actors[role])
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, req)
			want := http.StatusOK
			if role == "readonly" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Errorf("POST /tasks with a valid body as %s %s user: %d (%s), want %d", role, cred.name, w.Code, strings.TrimSpace(w.Body.String()), want)
			}
		}
	}
}
