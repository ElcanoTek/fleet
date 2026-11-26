// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/store"
)

// databaseOwnerWait is how long a starting `fleet serve` waits for another
// server that owns the database to let go before it refuses (ADR-0122). It
// covers a restart (systemd, a Helm Recreate rollout) whose previous process
// is still letting go — about half a minute when its host vanished without
// closing the connection (store.serverOwnerSessionSettings) — and is well
// inside the shipped unit's TimeoutStartSec. Waiting longer is the
// supervisor's restart loop's job, which retries anyway.
const databaseOwnerWait = 60 * time.Second

// errStoppedWaiting is takeTheDatabase's answer when a stop signal came while
// it waited: the server exits cleanly (exitQuietlyIfStopped).
var errStoppedWaiting = errors.New("stopped while waiting to take the database")

// exitQuietlyIfStopped is run's answer for takeTheDatabase's error: nil (a
// clean exit) for errStoppedWaiting, err itself otherwise.
func exitQuietlyIfStopped(err error) error {
	if errors.Is(err, errStoppedWaiting) {
		return nil
	}
	return err
}

// ownershipLostError is run's answer after it stopped because another server
// took the database over.
type ownershipLostError struct{ err error }

func (e ownershipLostError) Error() string {
	return fmt.Sprintf("stopped: %v (this server's owner lock connection dropped and another fleet serve holds the database since)", e.err)
}

func (e ownershipLostError) Unwrap() error { return e.err }

// takeTheDatabase makes this process the one fleet server running against
// the database, before it writes anything:
//
//  1. ownership (store.ClaimServerOwnership): a second `fleet serve` — run by
//     hand while the service runs, or a cloned box with the same
//     DATABASE_URL — refuses here, after up to wait, having recovered and
//     written nothing; its boot recovery would otherwise end the running
//     server's live turns, re-queue its claimed inputs and mark its
//     executing approvals outcome-unknown;
//  2. presence (store.HoldServerPresence): while it runs, `fleet admin rm` /
//     `fleet chat user del` refuse to delete an account (store.ExcludeServers),
//     and a CLI deletion already running is waited for, without a deadline.
//
// A stop signal while it waits returns errStoppedWaiting, holding nothing;
// once it returns, a stop signal is no longer caught here (it ends the
// process, as anywhere else before the listeners are up). release lets go of
// both locks, presence first. Boot recovery follows later (recoverAsOwner).
func takeTheDatabase(chatStore *store.Store, wait time.Duration) (owner *store.ServerOwnership, release func(), err error) {
	waitCtx, stopWaiting := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopWaiting()
	owner, err = chatStore.ClaimServerOwnership(waitCtx, store.ServerOwnershipOptions{Wait: wait})
	if err != nil {
		if waitCtx.Err() != nil {
			logging.From(waitCtx).Info("stopped while waiting for another fleet server to stop")
			return nil, nil, errStoppedWaiting
		}
		if errors.Is(err, store.ErrDatabaseOwned) {
			return nil, nil, fmt.Errorf("take the database: %w (stop the other fleet serve first, or point this one at a database of its own)", err)
		}
		return nil, nil, fmt.Errorf("take the database: %w", err)
	}
	releasePresence, err := chatStore.HoldServerPresence(waitCtx)
	if err != nil {
		owner.Release()
		if waitCtx.Err() != nil {
			logging.From(waitCtx).Info("stopped while waiting for an account deletion run from the CLI to finish")
			return nil, nil, errStoppedWaiting
		}
		return nil, nil, fmt.Errorf("mark the server as running: %w", err)
	}
	release = func() {
		releasePresence()
		owner.Release()
	}
	return owner, release, nil
}

// recoverAsOwner runs the boot recovery (recoverStrandedTurns) while this
// server still owns the database. A claim lost since takeTheDatabase — the
// presence wait has no deadline, and the boot in between takes time — means
// another server owns the database now and recovering would end its turns:
// the answer is then ownershipLostError, recovering nothing. It looks once
// more after recovery, before the scheduler store opens and writes.
func recoverAsOwner(owner ownership, chatStore *store.Store, inputQueueRetentionDays int) error {
	if err := ownershipLossErr(owner); err != nil {
		return err
	}
	recoverStrandedTurns(chatStore, inputQueueRetentionDays)
	return ownershipLossErr(owner)
}

// ownership is what the boot reads of its claim on the database
// (*store.ServerOwnership): Lost closes when the claim is lost to another
// server, and Err says why.
type ownership interface {
	Lost() <-chan struct{}
	Err() error
}

// ownedContext returns a context that ends the moment the claim is lost (or
// when stop is called). Everything the boot starts that writes on its own —
// a background backfill, the maintenance loop, the worker pool — runs under
// it, so a server that lost the database to another stops writing at once,
// not when the boot reaches the listener phase and its shutdown
// (stopOnOwnershipLoss) starts. The scheduler runs on a context of its own;
// stopSchedulerOnOwnershipLoss stops it on the same loss.
func ownedContext(owner ownership) (ctx context.Context, stop context.CancelFunc) {
	ctx, stop = context.WithCancel(context.Background())
	go func() {
		select {
		case <-owner.Lost():
			stop()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}

// asOwner runs one boot step that writes only while this server still owns
// the database. A lost claim is ownershipLostError, with step not run: the
// boot returns it, writing nothing more.
func asOwner(owner ownership, step func() error) error {
	if err := ownershipLossErr(owner); err != nil {
		return err
	}
	return step()
}

// ownershipLossErr is run's answer once it has shut down: why, when it stopped
// because another server took the database over, else nil.
func ownershipLossErr(owner ownership) error {
	if err := owner.Err(); err != nil {
		return ownershipLostError{err}
	}
	return nil
}

// stopSchedulerOnOwnershipLoss stops sch (once: its Stop is not idempotent)
// when the claim is lost — ctx, the boot's ownedContext, ends with a loss
// recorded — and returns the stop for run() to defer. The scheduler runs on a
// context of its own, so without this it would go on promoting tasks,
// recovering leases and firing chat schedules next to the new owner until
// run() returns, after the whole shutdown.
func stopSchedulerOnOwnershipLoss(ctx context.Context, owner ownership, sch interface{ Stop() }) (stop func()) {
	stop = sync.OnceFunc(sch.Stop)
	go func() {
		<-ctx.Done()
		if owner.Err() != nil {
			stop()
		}
	}()
	return stop
}

// stopOnOwnershipLoss sends the claim's loss to errCh, which shuts the server
// down at once (awaitShutdown), until ctx ends. ctx may itself end because of
// the loss (it derives from ownedContext), so a loss is looked for after it
// ends too, never dropped.
func stopOnOwnershipLoss(ctx context.Context, owner ownership, errCh chan<- error) {
	go func() {
		select {
		case <-owner.Lost():
		case <-ctx.Done():
			if owner.Err() == nil {
				return
			}
		}
		errCh <- ownershipLostError{owner.Err()}
	}()
}
