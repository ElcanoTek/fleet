package main

import (
	"context"
	"log"

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
// here too.
func startAccountEventsDelivery(ctx context.Context, cfg *config.Config, chatStore *store.Store) {
	if cfg.AccountEventsURL == "" {
		return
	}
	log.Printf("account events: delivering to the configured FLEET_ACCOUNT_EVENTS_URL")
	go accountevents.NewDeliverer(chatStore, cfg.AccountEventsURL, cfg.AccountEventsSecret).Run(ctx)
}
