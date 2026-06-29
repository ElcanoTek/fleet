package sandbox

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// persistent.go implements the opt-in per-conversation persistent sandbox
// lifecycle (#213). In persistent mode the run_python IPython kernel survives
// across turns WITHIN ONE conversation, so a DataFrame built in turn 1 is still
// in scope in turn 3. The isolation invariant is unchanged: a sandbox is keyed
// by conversation ID and is NEVER shared across conversations.
//
// The hard parts this file gets right:
//   - inUse refcount: the idle reaper / capacity eviction close a sandbox only
//     when no turn is borrowing it, so a long turn can't have its kernel pulled
//     out from under it. (httpapi.registerTurn cancels a prior same-conversation
//     turn, but cancellation is cooperative and the old turn may still be
//     unwinding when the new one starts — both briefly borrow the same entry.)
//   - remove-from-map-then-close, under the lock: a TakePersistent racing the
//     reaper can never receive a sandbox that is mid-Close.
//   - liveness probe + recreate: a persistent container that died between turns
//     (OOM-kill, host reap) is detected and replaced instead of wedging the
//     conversation on every subsequent turn.

// TakePersistent returns the sandbox bound to convID, creating it on first use
// and reusing it on every later turn of that conversation. The returned cleanup
// does NOT close the sandbox — it releases this turn's borrow and refreshes the
// idle timer. The sandbox is closed only by ReleaseChatSession, the idle reaper,
// capacity eviction, or Pool.Close.
//
// When persistent mode is disabled (or convID is empty, or the pool is shutting
// down) it degrades to the per-turn Take: a fresh sandbox whose cleanup closes
// it. That lets callers route through TakePersistent unconditionally.
func (p *Pool) TakePersistent(convID string) (*Sandbox, func(), error) {
	if p == nil || !p.cfg.PersistentREPL || convID == "" {
		return p.Take()
	}

	// Reuse path: claim an existing entry, then verify it's still alive. The
	// inUse++ happens under the lock BEFORE the probe so the reaper can't close
	// the sandbox while we probe it.
	for {
		p.persistentMu.Lock()
		if p.persistentClosed {
			p.persistentMu.Unlock()
			return p.Take()
		}
		e, ok := p.persistent[convID]
		if !ok || e.closeRequested {
			p.persistentMu.Unlock()
			break
		}
		e.inUse++
		e.lastUsed = p.now()
		p.persistentMu.Unlock()

		if p.sandboxAlive(e.sb) {
			return e.sb, p.borrow(e), nil
		}
		// Dead container: drop this entry (releasing our claim) and loop to
		// create a fresh one.
		p.retireDead(e)
	}

	// Create path: pull a (warm, if available) sandbox via Take and adopt it as
	// this conversation's persistent sandbox. Take's own cleanup is sb.Close,
	// which we intentionally drop — ownership transfers to the persistent map.
	sb, _, err := p.Take()
	if err != nil {
		return nil, func() {}, err
	}

	p.persistentMu.Lock()
	if p.persistentClosed {
		p.persistentMu.Unlock()
		sb.Close()
		return p.Take()
	}
	// A concurrent TakePersistent for the same conversation may have won the
	// race while we were constructing; prefer the already-registered one.
	if e, ok := p.persistent[convID]; ok && !e.closeRequested {
		e.inUse++
		e.lastUsed = p.now()
		p.persistentMu.Unlock()
		sb.Close()
		return e.sb, p.borrow(e), nil
	}
	e := &persistentEntry{sb: sb, convID: convID, lastUsed: p.now(), inUse: 1}
	p.persistent[convID] = e
	p.evictOverCapLocked(e)
	p.persistentMu.Unlock()
	return sb, p.borrow(e), nil
}

// borrow returns the per-turn cleanup for a claimed entry. It captures the
// entry POINTER (not the conversation ID) so a release always lands on the
// right entry even if the map slot was meanwhile replaced (e.g. after a dead
// container was retired). The last release of a closeRequested entry closes it.
func (p *Pool) borrow(e *persistentEntry) func() {
	return func() {
		p.persistentMu.Lock()
		e.inUse--
		e.lastUsed = p.now()
		shouldClose := e.inUse <= 0 && e.closeRequested
		if shouldClose {
			if cur, ok := p.persistent[e.convID]; ok && cur == e {
				delete(p.persistent, e.convID)
			}
		}
		p.persistentMu.Unlock()
		if shouldClose {
			e.sb.Close()
		}
	}
}

// retireDead releases this goroutine's claim on a dead entry and removes it from
// the map when no other turn still holds it. If another turn IS holding the
// (dead) sandbox, we mark it closeRequested so the last borrow release closes
// it; meanwhile the create path below installs a fresh entry under the same
// conversation key.
func (p *Pool) retireDead(e *persistentEntry) {
	p.persistentMu.Lock()
	e.inUse--
	removeAndClose := false
	if e.inUse <= 0 {
		if cur, ok := p.persistent[e.convID]; ok && cur == e {
			delete(p.persistent, e.convID)
		}
		removeAndClose = true
	} else {
		e.closeRequested = true
	}
	p.persistentMu.Unlock()
	if removeAndClose {
		e.sb.Close()
	}
}

// ReleaseChatSession closes and forgets a conversation's persistent sandbox.
// Called when a conversation is deleted. If a turn is mid-flight (inUse>0) the
// actual Close is deferred to the last borrow release (closeRequested) so we
// never yank the sandbox out from under a running turn. A no-op when persistent
// mode is off or the conversation has no live sandbox.
func (p *Pool) ReleaseChatSession(convID string) {
	if p == nil || convID == "" {
		return
	}
	p.persistentMu.Lock()
	e, ok := p.persistent[convID]
	if !ok {
		p.persistentMu.Unlock()
		return
	}
	if e.inUse > 0 {
		e.closeRequested = true
		p.persistentMu.Unlock()
		return
	}
	delete(p.persistent, convID)
	p.persistentMu.Unlock()
	e.sb.Close()
}

// evictOverCapLocked enforces PersistentMaxSessions by closing the
// least-recently-used IDLE entries until back under the cap. Called under
// persistentMu with the just-created entry passed as protect so it is never the
// one evicted. The slow Close runs in a goroutine so we don't hold the lock
// across a podman teardown.
func (p *Pool) evictOverCapLocked(protect *persistentEntry) {
	limit := p.cfg.PersistentMaxSessions
	if limit <= 0 || len(p.persistent) <= limit {
		return
	}
	type cand struct {
		key string
		e   *persistentEntry
	}
	idle := make([]cand, 0, len(p.persistent))
	for k, e := range p.persistent {
		if e == protect || e.inUse > 0 || e.closeRequested {
			continue
		}
		idle = append(idle, cand{key: k, e: e})
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].e.lastUsed.Before(idle[j].e.lastUsed) })
	for _, c := range idle {
		if len(p.persistent) <= limit {
			break
		}
		delete(p.persistent, c.key)
		sb := c.e.sb
		safe.Go("sandbox.pool.persistentEvict", sb.Close)
	}
}

// persistentKeeper reaps idle persistent sandboxes on a ticker so a conversation
// left untouched doesn't pin a container forever. Exits when done is closed (by
// Pool.Close).
func (p *Pool) persistentKeeper(done chan struct{}) {
	interval := p.cfg.PersistentIdleTTL / 2
	if interval <= 0 || interval > time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.reapIdlePersistent()
		}
	}
}

// reapIdlePersistent closes persistent sandboxes idle past PersistentIdleTTL.
// The recheck (inUse==0 && idle>ttl) and the map removal happen together under
// the lock; the Close happens AFTER removal and outside the lock. That ordering
// is what guarantees a TakePersistent racing the reaper can never be handed a
// sandbox that is mid-Close — it either finds the entry (and the reaper skips
// it because inUse>0) or doesn't (and creates a fresh one).
func (p *Pool) reapIdlePersistent() {
	ttl := p.cfg.PersistentIdleTTL
	if ttl <= 0 {
		return
	}
	now := p.now()
	var toClose []*Sandbox
	p.persistentMu.Lock()
	for k, e := range p.persistent {
		if e.inUse == 0 && !e.closeRequested && now.Sub(e.lastUsed) > ttl {
			delete(p.persistent, k)
			toClose = append(toClose, e.sb)
		}
	}
	p.persistentMu.Unlock()
	for _, sb := range toClose {
		sb.Close()
	}
}

// PersistentStats reports how many persistent conversation sandboxes are live
// and how many are currently idle (no in-flight turn). Both zero when
// persistent mode is off. Useful for the health summary (#301).
func (p *Pool) PersistentStats() (live, idle int) {
	if p == nil {
		return 0, 0
	}
	p.persistentMu.Lock()
	defer p.persistentMu.Unlock()
	live = len(p.persistent)
	for _, e := range p.persistent {
		if e.inUse == 0 {
			idle++
		}
	}
	return live, idle
}

// sandboxAlive cheaply probes whether a persistent sandbox's container is still
// usable, so a container that died between turns is recreated rather than
// handed back. Runs a trivial `true` through the bash path with a short
// deadline; any error (closed sandbox, vanished container) reads as not-alive.
func (p *Pool) sandboxAlive(sb *Sandbox) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sb.RunBash(ctx, BashRequest{Command: "true", Timeout: 5 * time.Second})
	if err != nil {
		log.Printf("sandbox: persistent sandbox failed liveness probe, recreating: %v", err)
		return false
	}
	return !res.TimedOut
}
