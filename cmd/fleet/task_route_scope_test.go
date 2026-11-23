package main

// Which tasks each per-task route reaches. A route's permission (the
// x-fleet-permission the OpenAPI probe pins) admits a principal to the route;
// a per-task scope then decides WHICH tasks. view_all_logs widens reading
// only: it reaches every task on the read routes, and the cancel route together
// with cancel_task, and never a route that changes a task, starts a run from
// it, feeds its run, or permanently deletes it (docs/TASK-OWNERSHIP.md).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// adminSession stands in for a credential in the scope probe: the request
// carries an admin-role session principal instead of an API key.
const adminSession = "admin-role session"

// taskRouteScope names the check that decides which tasks a route reaches.
type taskRouteScope string

const (
	// scopeRead: the principal's own tasks, or every task with view_all_logs
	// (taskVisibleToPrincipal, taskLogsReadable).
	scopeRead taskRouteScope = "read"
	// scopeFiles: the task's creator or the bootstrap admin key
	// (taskWorkspaceOwned). view_all_logs does not reach workspace bytes.
	scopeFiles taskRouteScope = "files"
	// scopeAct: the task's creator, or admin (taskActableByPrincipal).
	scopeAct taskRouteScope = "act"
	// scopeStop: the task's creator, or cancel_task on a task the principal
	// can read (taskStoppableByPrincipal). DELETE routes only, and only the
	// one that cancels: permanent delete is scopeAct (taskDeletableByPrincipal).
	scopeStop taskRouteScope = "stop"
	// scopeAdmin: PermissionAdmin, whatever the task.
	scopeAdmin taskRouteScope = "admin"
	// scopeList: a GET over many tasks, each filtered by scopeRead (the SQL
	// filter, or visibleTasks).
	scopeList taskRouteScope = "list"
	// scopeCreate: makes new tasks owned by the caller (or forecasts one); the
	// uploads it names follow checkCallerFiles.
	scopeCreate taskRouteScope = "create"
)

// adminOnlyGETs lists the GET routes on the task surface that only admin may
// call, each with its reason. They may carry scopeAdmin; every other GET must
// be a read scope. None today.
var adminOnlyGETs = map[string]string{}

// taskRouteScopes classifies every orchestrator route on the task surface:
// every route under /tasks or /logs, and every route with a path parameter
// naming a task. A new one fails TestEveryTaskRouteHasAScope until it is
// listed here, and TestViewAllLogsReachesOnlyReadAndStopRoutes then drives
// each {task_id} route. The list, create and admin collection routes are
// covered by the visibility, upload and OpenAPI permission tests.
var taskRouteScopes = map[string]taskRouteScope{
	"GET /tasks":            scopeList,
	"GET /tasks/upcoming":   scopeList,
	"GET /tasks/tags":       scopeList,
	"GET /tasks/recipients": scopeList,
	"GET /tasks/export":     scopeList,
	"POST /tasks":           scopeCreate,
	"POST /tasks/batch":     scopeCreate,
	"POST /tasks/estimate":  scopeCreate,
	"POST /tasks/import":    scopeCreate,
	"POST /tasks/cleanup":   scopeAdmin,
	"POST /tasks/model":     scopeAdmin,

	"GET /tasks/{task_id}":                   scopeRead,
	"GET /tasks/{task_id}/output":            scopeRead,
	"GET /tasks/{task_id}/error-analysis":    scopeRead,
	"GET /tasks/{task_id}/stream":            scopeRead,
	"GET /logs/{task_id}":                    scopeRead,
	"GET /logs/{task_id}/history":            scopeRead,
	"GET /logs/{task_id}/history/{entry_id}": scopeRead,
	"GET /tasks/{task_id}/artifacts":         scopeFiles,
	"GET /tasks/{task_id}/workspace":         scopeFiles,
	"GET /tasks/{task_id}/workspace/*":       scopeFiles,
	"PUT /tasks/{task_id}":                   scopeAct,
	"POST /tasks/{task_id}/tags":             scopeAct,
	"POST /tasks/{task_id}/rerun":            scopeAct,
	"POST /tasks/{task_id}/clone":            scopeAct,
	"POST /tasks/{task_id}/wake":             scopeAct,
	"DELETE /tasks/{task_id}":                scopeStop,
	"DELETE /tasks/{task_id}/permanent":      scopeAct,
	"PUT /tasks/{task_id}/owner":             scopeAdmin,
}

// pathParams returns the names of a chi route pattern's {params}.
func pathParams(route string) []string {
	matches := regexp.MustCompile(`\{([^}]+)\}`).FindAllStringSubmatch(route, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// onTaskSurface reports whether a live route belongs in taskRouteScopes: its
// path is under /tasks or /logs, or one of its path parameters names a task.
func onTaskSurface(route string) bool {
	return underTasksOrLogs(route) ||
		slices.ContainsFunc(pathParams(route), func(p string) bool { return strings.Contains(strings.ToLower(p), "task") })
}

// underTasksOrLogs reports whether a route's path is /tasks, or under /tasks/
// or /logs/.
func underTasksOrLogs(route string) bool {
	_, path, _ := strings.Cut(route, " ")
	return path == "/tasks" || strings.HasPrefix(path, "/tasks/") || strings.HasPrefix(path, "/logs/")
}

// perTaskScope reports whether scope decides access to one named task, so its
// route must name that task as {task_id}; the list and create scopes cover
// routes that name no task, and admin may cover either.
func perTaskScope(scope taskRouteScope) bool {
	switch scope {
	case scopeRead, scopeFiles, scopeAct, scopeStop:
		return true
	default:
		return false
	}
}

// TestEveryTaskRouteHasAScope walks the real router: every route under /tasks
// and /logs, and every route with a path parameter naming a task, is
// classified; every classified route and every adminOnlyGETs entry exists; a
// /tasks or /logs route's first path parameter, and any parameter naming a
// task, is spelled {task_id}, and a per-task scope's route names {task_id},
// so the probe below drives every route that reaches one task; only safe
// methods use the read scopes (an admin-only GET only when it is in
// adminOnlyGETs); and only DELETE uses the stop scope. So a route that changes
// something reaches view_all_logs's reading grant only by cancelling a task.
func TestEveryTaskRouteHasAScope(t *testing.T) {
	t.Parallel()

	live := liveRoutes(t)
	for route := range live {
		if !onTaskSurface(route) {
			continue
		}
		if _, ok := taskRouteScopes[route]; !ok {
			t.Errorf("%s is on the task surface but has no entry in taskRouteScopes: decide which tasks it reaches (read, list, files, act, stop, create or admin) and add it", route)
		}
		params := pathParams(route)
		for _, p := range params {
			if strings.Contains(strings.ToLower(p), "task") && p != "task_id" {
				t.Errorf("%s names a task as {%s}: spell it {task_id} so TestViewAllLogsReachesOnlyReadAndStopRoutes drives it", route, p)
			}
		}
		if underTasksOrLogs(route) && len(params) > 0 && params[0] != "task_id" {
			t.Errorf("%s: a /tasks or /logs route names its task as {task_id}, its first path parameter, so TestViewAllLogsReachesOnlyReadAndStopRoutes drives it (got {%s})", route, params[0])
		}
	}
	for route, reason := range adminOnlyGETs {
		if !live[route] {
			t.Errorf("adminOnlyGETs lists %s (%s), which the router does not serve", route, reason)
		}
		if scope := taskRouteScopes[route]; scope != scopeAdmin {
			t.Errorf("adminOnlyGETs lists %s, whose scope is %q, not admin", route, scope)
		}
	}
	for route, scope := range taskRouteScopes {
		if !live[route] {
			t.Errorf("taskRouteScopes lists %s, which the router does not serve", route)
		}
		method, _, _ := strings.Cut(route, " ")
		safe := method == http.MethodGet || method == http.MethodHead
		readScope := scope == scopeRead || scope == scopeList || scope == scopeFiles
		_, adminGET := adminOnlyGETs[route]
		switch {
		case safe && !readScope && (!adminGET || scope != scopeAdmin):
			t.Errorf("%s has scope %q: a GET route needs a read scope (read, list, files), or scopeAdmin and an entry in adminOnlyGETs", route, scope)
		case !safe && readScope:
			t.Errorf("%s has scope %q: the read scopes are for GET routes, and every other method needs act, stop, create or admin", route, scope)
		}
		if scope == scopeStop && method != http.MethodDelete {
			t.Errorf("%s has scope %q: stop is for DELETE routes; a route that changes or runs the task needs act", route, scope)
		}
		if names := strings.Contains(route, "{task_id}"); scope != scopeAdmin && perTaskScope(scope) != names {
			t.Errorf("%s has scope %q: the read, files, act and stop scopes are for routes naming {task_id}, and list and create for routes naming none", route, scope)
		}
	}
}

// TestViewAllLogsReachesOnlyReadAndStopRoutes drives every classified route
// through the real router with a legacy key holding every permission except
// admin — view_all_logs, create_task and cancel_task included — against a task
// it did not create, then against one it did:
//
//   - on another principal's task it is admitted to the read and stop routes,
//     and refused (403) on the files, act (permanent delete included) and
//     admin routes;
//   - on its own task it is admitted everywhere but the admin route;
//   - the bootstrap admin key is admitted on another principal's task
//     everywhere, and a typed admin key and an admin-role session on every
//     act, stop and admin route.
//
// "Admitted" is the probe's meaning: neither 401 nor 403. Each request gets a
// fresh terminal task, so a stop or an edit never changes the next request's
// row.
func TestViewAllLogsReachesOnlyReadAndStopRoutes(t *testing.T) {
	env := newProbeEnv(t)
	prefix := specServerPrefix(t)
	header := specAPIKeyHeader(t)

	var perms []models.Permission
	for _, p := range models.AllPermissions {
		if p != models.PermissionAdmin {
			perms = append(perms, p)
		}
	}
	readerKey, readerRaw, err := env.keyMgr.CreateKey(context.Background(), "reader-"+uuid.NewString(), perms, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}
	_, typedAdminRaw, err := env.keyMgr.CreateTypedKey(context.Background(), "typed-admin-"+uuid.NewString(), apikeys.KeyTypeAdmin, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create typed admin key: %v", err)
	}
	adminAccount, err := env.store.Identity().CreateAccount(context.Background(),
		identity.NewAccount{Email: "scope-admin-" + uuid.NewString()[:8] + "@probe.example", Role: identity.RoleAdmin})
	if err != nil {
		t.Fatalf("add admin user: %v", err)
	}
	otherKeyID := "another-key"

	send := func(t *testing.T, route, raw string, createdByKey *string) int {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "task route scope probe", Status: models.TaskStatusSuccess,
			CreatedAt: time.Now().UTC(), CreatedByKeyID: createdByKey}
		if _, err := env.store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		method, path, _ := strings.Cut(route, " ")
		body := ""
		if method == http.MethodPost || method == http.MethodPut {
			body = "{}"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(method, prefix+fillPath(path, task.ID.String()), strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		if raw == adminSession {
			// What the gateway hands an in-process call for a signed-in admin
			// (ADR-0108); the Origin header is what a browser sends (CSRF).
			req = req.WithContext(principal.With(ctx, principal.Principal{Email: adminAccount.Email, AuthMethod: principal.MethodSession}))
			req.Header.Set("Origin", "http://"+req.Host)
		} else {
			req.Header.Set(header, raw)
		}
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		return w.Code
	}
	admitted := func(code int) bool { return code != http.StatusUnauthorized && code != http.StatusForbidden }

	routes := make([]string, 0, len(taskRouteScopes))
	for route := range taskRouteScopes {
		routes = append(routes, route)
	}
	slices.Sort(routes)
	for _, route := range routes {
		scope := taskRouteScopes[route]
		if !strings.Contains(route, "{task_id}") {
			continue // a collection route: no one task to probe
		}
		t.Run(route, func(t *testing.T) {
			foreign := send(t, route, readerRaw, &otherKeyID)
			switch scope {
			case scopeRead, scopeStop:
				if !admitted(foreign) {
					t.Errorf("view_all_logs key on another principal's task (%s scope) = %d, want admitted", scope, foreign)
				}
			default:
				if foreign != http.StatusForbidden {
					t.Errorf("view_all_logs key on another principal's task (%s scope) = %d, want 403", scope, foreign)
				}
			}

			own := send(t, route, readerRaw, &readerKey.KeyID)
			if scope == scopeAdmin {
				if own != http.StatusForbidden {
					t.Errorf("non-admin key on its own task (admin scope) = %d, want 403", own)
				}
			} else if !admitted(own) {
				t.Errorf("key on its own task (%s scope) = %d, want admitted", scope, own)
			}

			if code := send(t, route, env.adminKey, &otherKeyID); !admitted(code) {
				t.Errorf("bootstrap admin key on another principal's task = %d, want admitted", code)
			}
			if scope == scopeAct || scope == scopeStop || scope == scopeAdmin {
				for name, raw := range map[string]string{"typed admin key": typedAdminRaw, "admin-role session": adminSession} {
					if code := send(t, route, raw, &otherKeyID); !admitted(code) {
						t.Errorf("%s on another principal's task (%s scope) = %d, want admitted", name, scope, code)
					}
				}
			}
		})
	}
}
