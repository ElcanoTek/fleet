package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// accountEventsRecorder builds the account-events recorder when the feed is
// configured (docs/ACCOUNT-EVENTS.md), else nil: with FLEET_ACCOUNT_EVENTS_URL
// unset nothing is ever queued. config.Load already refused a URL without
// its signing secret.
func accountEventsRecorder(cfg *config.Config, chatStore *store.Store, schedStorage *storage.Storage) *accountevents.Recorder {
	if cfg.AccountEventsURL == "" {
		return nil
	}
	return accountevents.NewRecorder(chatStore, schedStorage, chatStore)
}

// startAccountEventsDelivery drains the outbox in the background for the life
// of ctx. Rows queued by the operator CLI (a separate process) are delivered
// here too. With the feed off it still runs the retention sweep: a feed that
// was on and then switched off must not keep its delivered and given-up rows
// past the documented 7 and 30 days.
func startAccountEventsDelivery(ctx context.Context, cfg *config.Config, chatStore *store.Store) {
	if cfg.AccountEventsURL != "" {
		log.Printf("account events: delivering to the configured FLEET_ACCOUNT_EVENTS_URL")
	}
	go accountevents.NewDeliverer(chatStore, cfg.AccountEventsURL, cfg.AccountEventsSecret).Run(ctx)
}

// accountEventsCreateUser wraps the orchestrator admin API's `POST /users`
// (handlers.CreateUser, ADMIN_API_KEY-gated) for the account-events feed. That
// route writes an Operations Center identity directly, so when its username is
// a Chat account's email it changes that account's effective ops_role like
// any other writer — and must publish like one (source "cli": an operator
// credential, not a person in the admin UI). A username that is no Chat
// account publishes nothing, as the Recorder's before/after comparison
// already guarantees. With the feed off (nil recorder) the handler is
// returned as is.
func accountEventsCreateUser(next http.HandlerFunc, events *accountevents.Recorder) http.HandlerFunc {
	if events == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Peek the username without consuming the body the handler decodes. The
		// handler bounds nothing itself, so the same 1 MiB cap applies to both.
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var peek struct {
			Username string `json:"username"`
		}
		if json.Unmarshal(raw, &peek) != nil || strings.TrimSpace(peek.Username) == "" {
			next(w, r)
			return
		}
		beginCtx, cancel := context.WithTimeout(r.Context(), accountEventTimeout)
		change := events.Begin(beginCtx, peek.Username)
		cancel()
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), accountEventTimeout)
			defer cancel()
			change.CommitLogged(ctx, store.AccountEventSourceCLI, "")
		}()
		next(w, r)
	}
}

// accountEventTimeout bounds each half of the feed's work on a request.
const accountEventTimeout = 10 * time.Second
