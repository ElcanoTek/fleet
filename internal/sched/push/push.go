// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package push delivers A2A push notifications (#1279 Phase 2): when a task
// with registered push configs changes caller-visible state, POST an A2A
// StreamResponse to each config's webhook URL.
//
// The dispatcher is POLL-BASED, not transition-hooked: the task ROW is
// already the A2A event source (the SSE streams and the blocking unary wait
// poll it), and a Storage-level transition observer would touch every one of
// the ~14 lifecycle writers plus three coupling-test matrices to gain
// sub-second latency the spec does not ask for. One scan per tick lists the
// configs whose task status moved past their delivery marker
// (db.ListA2APushWork), delivers, and marks the ATTEMPT — the A2A spec's
// only delivery MUST is "at least once attempt per configured webhook";
// retries are a MAY this version deliberately skips, and duplicates (a crash
// between POST and mark) are explicitly permitted ("duplicate deliveries may
// occur"). Fast intermediate flaps between ticks collapse into the net
// change, documented in docs/A2A.md.
//
// Security posture: the webhook URL is CALLER-supplied, so it sits in the
// same trust class as web_fetch targets, not the operator-trusted
// FLEET_WEBHOOK_URL. Delivery uses a resolved-IP dial-time SSRF guard
// (mcpoauth.SafeHTTPClient — netguard-backed, covers DNS rebinding and every
// redirect hop) and refuses redirects outright, because a 30x would relay
// the caller's own Authorization header to a different origin. The
// FLEET_A2A_PUSH_ALLOW_PRIVATE escape hatch (default off) admits
// loopback/private receivers for development and TCK conformance runs —
// the official TCK's receiver listens on localhost; redirects stay refused
// even then.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// deliverTimeout bounds one webhook POST — the spec recommends 10–30s.
const deliverTimeout = 10 * time.Second

// scanInterval matches the A2A streams' poll cadence: task-level transitions
// are seconds apart, so 1s keeps notifications timely without hammering the
// database (the scan is one indexed join that is empty on deployments with
// no registered configs).
const scanInterval = time.Second

// scanBatch bounds one tick's deliveries; the next tick takes the remainder.
const scanBatch = 100

// notificationTokenHeaders carries the config's token to the receiver. The
// spec never names a header for it; these are the two de-facto spellings —
// X-A2A-Notification-Token (a2a-python, a2a-js, a2a-java receivers) and
// A2A-Notification-Token (a2a-go). Sending both costs a few bytes and
// interoperates with all four SDKs.
var notificationTokenHeaders = []string{"X-A2A-Notification-Token", "A2A-Notification-Token"}

// Store is the persistence surface the dispatcher needs; *storage.Storage
// satisfies it.
type Store interface {
	ListA2APushWork(ctx context.Context, limit int) ([]models.A2APushWork, error)
	MarkA2APushAttempted(ctx context.Context, taskID uuid.UUID, configID string, status models.TaskStatus) (bool, error)
}

// Dispatcher owns the scan-deliver-mark loop.
type Dispatcher struct {
	store  Store
	client *http.Client
	logf   func(format string, args ...any)
}

// New builds a dispatcher. allowPrivate disables the SSRF dial guard (never
// the redirect refusal) — the FLEET_A2A_PUSH_ALLOW_PRIVATE posture for
// dev/TCK runs against loopback receivers.
func New(store Store, allowPrivate bool) *Dispatcher {
	client := mcpoauth.SafeHTTPClient(deliverTimeout)
	if allowPrivate {
		client = &http.Client{
			Timeout: deliverTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("a2a push: redirect refused")
			},
		}
	}
	return &Dispatcher{store: store, client: client, logf: log.Printf}
}

// Run scans until ctx ends. Deliveries within a tick run sequentially — the
// batch bound keeps a tick finite, and per-receiver parallelism is not worth
// its complexity until a real integrator needs it.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		d.tick(ctx)
	}
}

// tick performs one scan-deliver-mark pass.
func (d *Dispatcher) tick(ctx context.Context) {
	work, err := d.store.ListA2APushWork(ctx, scanBatch)
	if err != nil {
		if ctx.Err() == nil {
			d.logf("a2a push: work scan failed: %v", err)
		}
		return
	}
	for _, w := range work {
		if ctx.Err() != nil {
			return
		}
		// Mark FIRST, conditionally: the one-winner UPDATE is what keeps a
		// second dispatcher (or an overlapping tick) from double-delivering
		// the same transition. Losing the race means someone else owns this
		// delivery. A crash after marking loses at most one notification for
		// a state the next transition supersedes — while the reverse order
		// would deliver duplicates on every crash AND on every race, and the
		// client contract already demands idempotent processing.
		claimed, err := d.store.MarkA2APushAttempted(ctx, w.Config.TaskID, w.Config.ID, w.Status)
		if err != nil {
			d.logf("a2a push: mark failed for task %s config %s: %v", w.Config.TaskID, w.Config.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		if err := d.deliver(ctx, w); err != nil {
			// One attempt is the contract (spec: retry is MAY). The receiver
			// learns current state from GetTask; the next transition pushes
			// again. URL and credentials never enter the log line.
			d.logf("a2a push: delivery attempt failed for task %s config %s: %v",
				w.Config.TaskID, w.Config.ID, sanitizeErr(err))
		}
	}
}

// deliver POSTs one StreamResponse-wrapped statusUpdate — the doorbell, not
// the data: the spec's own async guidance is that receivers re-fetch the
// full task via GetTask on notification.
func (d *Dispatcher) deliver(ctx context.Context, w models.A2APushWork) error {
	state, _ := a2abridge.TaskStateFor(w.Status)
	now := time.Now().UTC()
	event := &wire.TaskStatusUpdateEvent{
		TaskID:    wire.TaskID(w.Config.TaskID.String()),
		ContextID: w.Config.TaskID.String(),
		Status:    wire.TaskStatus{State: state, Timestamp: &now},
	}
	body, err := json.Marshal(wire.StreamResponse{Event: event})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.Config.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// The spec's media type for push payloads (§4.3.3 / §14.1.1).
	req.Header.Set("Content-Type", "application/a2a+json")
	if w.Config.Token != "" {
		for _, h := range notificationTokenHeaders {
			req.Header.Set(h, w.Config.Token)
		}
	}
	// Authorization is rendered VERBATIM as "{scheme} {credentials}": the
	// scheme string round-trips exactly as the caller registered it, because
	// receivers (the official TCK included) compare the header by string.
	if w.Config.AuthScheme != "" && w.Config.AuthCredentials != "" {
		req.Header.Set("Authorization", w.Config.AuthScheme+" "+w.Config.AuthCredentials)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// sanitizeErr strips the URL a transport error may echo — the webhook URL is
// caller data and can carry secrets in its path or query.
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	// url.Error formats as `Post "http://…": cause`; report only the cause.
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok && u.Unwrap() != nil {
		return u.Unwrap().Error()
	}
	return err.Error()
}
