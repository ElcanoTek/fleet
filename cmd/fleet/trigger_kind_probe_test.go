package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// triggerRouteKind names the one trigger kind each trigger operation fires. A
// trigger operation (x-fleet-permission webhook or hmac) without a row here
// fails TestOpenAPITriggerRoutesFireOnlyTheirKind, so a new trigger route has
// to state its kind before it ships, and is then probed against every other
// kind in models.TriggerKinds.
var triggerRouteKind = map[string]models.TriggerKind{
	"POST /triggers/{slug}":       models.TriggerKindWebhook,
	"POST /triggers/email/{slug}": models.TriggerKindEmail,
}

// TestOpenAPITriggerRoutesFireOnlyTheirKind proves, on the real stack with
// real trigger rows, that every trigger route answers a trigger of any other
// kind exactly as it answers an unknown slug — the same status and body — and
// spawns no run, for every credential form the route accepts: a valid
// signature always, and for the webhook route also a webhook key listing the
// slug (as X-API-Key and as a bearer). Each route's own kind is admitted by
// TestOpenAPITriggerAuthEnforcement; this test covers the cross-kind pairs, so
// a kind added to models.TriggerKinds is probed on every route automatically.
func TestOpenAPITriggerRoutesFireOnlyTheirKind(t *testing.T) {
	env := newProbeEnv(t)
	prefix := specServerPrefix(t)
	keyHeader := specSchemeHeader(t, "webhookKey")
	sigHeader := specSchemeHeader(t, "triggerSignature")

	const secret = "openapi-probe-kind-secret-0123456789abcdef"
	body := []byte(`{"message_id":"<kind-probe@corp.example>","from":"alerts@corp.example","subject":"probe","text":"probe","dkim":"pass","spf":"pass"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	probeSlug := func() string { return "kind-probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
	newTrigger := func(kind models.TriggerKind) string {
		t.Helper()
		template := &models.Task{ID: uuid.New(), Prompt: "trigger kind probe template", Status: models.TaskStatusScheduled,
			Priority: models.PriorityNormal, TriggerType: models.TriggerTypeWebhook, AllowEventTriggers: true,
			MCPSelection: models.MCPSelection{{Server: "github", Account: "default"}}, CreatedAt: time.Now().UTC()}
		if _, err := env.store.AddTask(template); err != nil {
			t.Fatalf("add template task: %v", err)
		}
		slug := probeSlug()
		trig := &models.TaskTrigger{ID: uuid.New(), TaskID: template.ID, Slug: slug, Secret: secret, Kind: kind}
		if kind == models.TriggerKindEmail {
			// A policy the probe body satisfies, so only the route decides.
			trig.EmailPolicy = &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.example"}, RequireDKIM: true}
		}
		if err := env.store.CreateTrigger(context.Background(), trig); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
		return slug
	}
	taskCount := func() int {
		t.Helper()
		_, total, err := env.store.GetTasksFiltered(scheddb.TaskFilter{}, 1, 0)
		if err != nil {
			t.Fatalf("count tasks: %v", err)
		}
		return total
	}
	send := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, prefix+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		return w
	}

	fired := map[models.TriggerKind]bool{}
	seen := map[string]bool{}
	for _, op := range specOps(t) {
		if op.permission != permWebhook && op.permission != permHMAC {
			continue
		}
		seen[op.String()] = true
		accepts, ok := triggerRouteKind[op.String()]
		if !ok {
			t.Errorf("%s is a trigger operation with no triggerRouteKind row: name the one kind it fires", op)
			continue
		}
		fired[accepts] = true
		for _, kind := range models.TriggerKinds {
			if kind == accepts {
				continue
			}
			t.Run(op.String()+"/"+string(kind)+"-kind trigger", func(t *testing.T) {
				slug, unknown := newTrigger(kind), probeSlug()
				forms := map[string]map[string]string{"valid signature": {sigHeader: signature}}
				if op.permission == permWebhook {
					_, key, err := env.keyMgr.CreateTypedKey(context.Background(), "kind-probe-"+uuid.NewString(), apikeys.KeyTypeWebhook, []string{slug, unknown}, 0, nil, "")
					if err != nil {
						t.Fatalf("create webhook key: %v", err)
					}
					forms["webhook key listing the slug"] = map[string]string{keyHeader: key}
					forms["webhook key listing the slug, as bearer"] = map[string]string{"Authorization": "Bearer " + key}
				}
				for name, headers := range forms {
					before := taskCount()
					got := send(strings.ReplaceAll(op.path, "{slug}", slug), headers)
					want := send(strings.ReplaceAll(op.path, "{slug}", unknown), headers)
					if want.Code != http.StatusUnauthorized {
						t.Fatalf("%s: unknown slug answered %d, want 401", name, want.Code)
					}
					if got.Code != want.Code || got.Body.String() != want.Body.String() {
						t.Errorf("%s: the slug of a trigger of kind %q answered %d %q; an unknown slug answers %d %q",
							name, kind, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
					}
					if after := taskCount(); after != before {
						t.Errorf("%s: %d run(s) spawned for a trigger of kind %q", name, after-before, kind)
					}
				}
			})
		}
	}
	for route := range triggerRouteKind {
		if !seen[route] {
			t.Errorf("triggerRouteKind names %s, which is not a trigger operation in docs/openapi.yaml", route)
		}
	}
	for _, kind := range models.TriggerKinds {
		if !fired[kind] {
			t.Errorf("trigger kind %q is fired by no trigger operation", kind)
		}
	}
}
