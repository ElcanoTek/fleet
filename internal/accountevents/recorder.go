// Package accountevents publishes Fleet's signed account-events feed
// (docs/ACCOUNT-EVENTS.md): whenever a Chat account's membership or roles
// change, the full resulting state is queued in a durable outbox and POSTed,
// HMAC-signed, to one operator-configured URL. Fleet names no receiver: any
// identity provider, audit sink or script can subscribe.
//
// The Recorder is shared by every writer (the admin HTTP API, the operator CLI,
// boot seeding, and changes applied from the identity provider) so "what
// changed" is computed one way: snapshot before, read back after across both
// the Chat and Ops databases, and queue only a real difference.
package accountevents

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ElcanoTek/fleet/internal/store"
)

// ChatReader reads the Chat plane's view of one account (centrally disabled
// rows included).
type ChatReader interface {
	AccountAccess(ctx context.Context, email string) (store.AccountAccess, bool, error)
}

// OpsRoleReader resolves an account's effective Operations Center role:
// "none" when it has no enabled Ops identity.
type OpsRoleReader interface {
	OpsRole(ctx context.Context, email string) (string, error)
}

// Queue is the durable outbox.
type Queue interface {
	EnqueueAccountEvent(ctx context.Context, ev store.AccountEvent) (store.AccountEvent, error)
}

// State is one account's access as the feed reports it.
type State struct {
	Exists   bool
	Enabled  bool
	ChatRole string
	OpsRole  string
}

// Recorder turns before/after snapshots into outbox rows. A nil *Recorder is
// the feature switched off: every method is a no-op, so callers never branch
// on configuration.
type Recorder struct {
	chat  ChatReader
	ops   OpsRoleReader
	queue Queue
}

// NewRecorder wires a Recorder. ops may be nil (no Operations Center plane);
// every account then reports ops_role "none".
func NewRecorder(chat ChatReader, ops OpsRoleReader, queue Queue) *Recorder {
	return &Recorder{chat: chat, ops: ops, queue: queue}
}

// Snapshot reads email's current state across both planes.
func (r *Recorder) Snapshot(ctx context.Context, email string) (State, error) {
	if r == nil {
		return State{}, nil
	}
	access, found, err := r.chat.AccountAccess(ctx, email)
	if err != nil {
		return State{}, fmt.Errorf("read chat account: %w", err)
	}
	if !found {
		return State{}, nil
	}
	opsRole := "none"
	if r.ops != nil {
		if opsRole, err = r.ops.OpsRole(ctx, email); err != nil {
			return State{}, fmt.Errorf("read ops role: %w", err)
		}
	}
	return State{Exists: true, Enabled: access.Enabled, ChatRole: access.Role, OpsRole: opsRole}, nil
}

// Change is an operation in progress: the state captured before it ran.
type Change struct {
	r      *Recorder
	email  string
	before State
	err    error
}

// Begin snapshots email before a change. A failed snapshot is carried to
// Commit, which then refuses to guess and reports it (an event computed from an
// unknown "before" could be a false removal or a missed change).
func (r *Recorder) Begin(ctx context.Context, email string) *Change {
	if r == nil {
		return nil
	}
	before, err := r.Snapshot(ctx, email)
	return &Change{r: r, email: email, before: before, err: err}
}

// ErrNotQueued wraps every reason Commit could not queue an event, so callers
// can tell the operator that `fleet account-events resync` will repair it.
var ErrNotQueued = errors.New("account event not queued")

// Commit reads email back after the change and queues one event when the
// resulting state differs from the snapshot. It never undoes or fails the change
// itself: callers log (HTTP) or print (CLI) a returned error and carry on.
func (c *Change) Commit(ctx context.Context, source, actor string) error {
	if c == nil {
		return nil
	}
	if c.err != nil {
		return fmt.Errorf("%w for %s: %w", ErrNotQueued, c.email, c.err)
	}
	after, err := c.r.Snapshot(ctx, c.email)
	if err != nil {
		return fmt.Errorf("%w for %s: %w", ErrNotQueued, c.email, err)
	}
	if after == c.before {
		return nil
	}
	if !after.Exists {
		// Created and deleted inside one operation, or never a Chat account.
		if !c.before.Exists {
			return nil
		}
		return c.r.enqueue(ctx, c.email, after, store.AccountEventDeleted, source, actor)
	}
	return c.r.enqueue(ctx, c.email, after, store.AccountEventAccessChanged, source, actor)
}

// CommitLogged is Commit for request paths: a failure is logged, not returned.
func (c *Change) CommitLogged(ctx context.Context, source, actor string) {
	if err := c.Commit(ctx, source, actor); err != nil {
		log.Printf("account events: %v (run `fleet account-events resync` to republish)", err)
	}
}

// Publish queues email's current state unconditionally (resync). Accounts that
// no longer exist are skipped.
func (r *Recorder) Publish(ctx context.Context, email, source, actor string) (bool, error) {
	if r == nil {
		return false, nil
	}
	st, err := r.Snapshot(ctx, email)
	if err != nil {
		return false, err
	}
	if !st.Exists {
		return false, nil
	}
	return true, r.enqueue(ctx, email, st, store.AccountEventAccessChanged, source, actor)
}

func (r *Recorder) enqueue(ctx context.Context, email string, st State, typ, source, actor string) error {
	ev := store.AccountEvent{Type: typ, Email: email, Source: source, Actor: actor}
	if typ == store.AccountEventAccessChanged {
		ev.Enabled, ev.ChatRole, ev.OpsRole = st.Enabled, st.ChatRole, st.OpsRole
	}
	if _, err := r.queue.EnqueueAccountEvent(ctx, ev); err != nil {
		return fmt.Errorf("%w for %s: %w", ErrNotQueued, email, err)
	}
	return nil
}
