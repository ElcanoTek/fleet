// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// accountGate tracks the requests each account has in flight, so deleting an
// account can stop them before its rows go: a request that passed the
// membership check just before the deletion would otherwise finish after it
// and write rows (a memory proposal, a push subscription) that a later
// account under the same email would read as its own.
//
// membershipMiddleware enters the gate on the request's email before it
// resolves the account; close refuses further requests for the email and
// cancels the contexts of those in flight, and wait blocks until they have
// returned. The zero value is ready.
type accountGate struct {
	mu     sync.Mutex
	closed map[string]int // email → closers holding it shut
	active map[string]map[*gateRequest]struct{}
	// changed is broadcast whenever a request ends, a wait's deadline passes
	// or its context ends; every waiter re-checks its own email. One
	// condition for all emails, so two deletions of one account waiting at
	// once are both woken.
	changed *sync.Cond
}

type gateRequest struct{ cancel context.CancelFunc }

// gateKey is the email as accounts are keyed (identity.NormalizeEmail), so a
// deletion and a request name the same entry however the address is typed.
func gateKey(email string) string { return identity.NormalizeEmail(email) }

// condLocked returns the gate's condition; g.mu is held.
func (g *accountGate) condLocked() *sync.Cond {
	if g.changed == nil {
		g.changed = sync.NewCond(&g.mu)
	}
	return g.changed
}

// enter admits one request for email, returning the context to serve it with
// and the function that ends it. ok is false while the email is closed.
func (g *accountGate) enter(ctx context.Context, email string) (_ context.Context, leave func(), ok bool) {
	key := gateKey(email)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed[key] > 0 {
		return ctx, func() {}, false
	}
	ctx, cancel := context.WithCancel(ctx)
	req := &gateRequest{cancel: cancel}
	if g.active == nil {
		g.active = map[string]map[*gateRequest]struct{}{}
	}
	if g.active[key] == nil {
		g.active[key] = map[*gateRequest]struct{}{}
	}
	g.active[key][req] = struct{}{}
	return ctx, func() {
		cancel()
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.active[key], req)
		if len(g.active[key]) == 0 {
			delete(g.active, key)
			g.condLocked().Broadcast()
		}
	}, true
}

// isClosed reports whether a deletion holds email's gate shut.
func (g *accountGate) isClosed(email string) bool {
	key := gateKey(email)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed[key] > 0
}

// close refuses new requests for email and cancels the ones in flight. The
// returned function reopens the gate; until then every request for email is
// refused as not a member.
func (g *accountGate) close(email string) (reopen func()) {
	key := gateKey(email)
	g.mu.Lock()
	if g.closed == nil {
		g.closed = map[string]int{}
	}
	g.closed[key]++
	for req := range g.active[key] {
		req.cancel()
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.closed[key]--; g.closed[key] <= 0 {
				delete(g.closed, key)
			}
		})
	}
}

// wait blocks until email has no request in flight, timeout passes or ctx
// ends. It reports whether the requests drained; err is ctx's error when ctx
// ended first.
func (g *accountGate) wait(ctx context.Context, email string, timeout time.Duration) (drained bool, err error) {
	key := gateKey(email)
	deadline := time.Now().Add(timeout)
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.condLocked()
	// sync.Cond has no timed or cancellable wait: a timer and the context
	// broadcast instead.
	wake := func() {
		g.mu.Lock()
		c.Broadcast()
		g.mu.Unlock()
	}
	t := time.AfterFunc(timeout, wake)
	defer t.Stop()
	stop := context.AfterFunc(ctx, wake)
	defer stop()
	for len(g.active[key]) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		c.Wait()
	}
	return true, nil
}
