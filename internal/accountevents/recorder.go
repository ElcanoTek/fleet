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
	"strings"

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

// BaselineAdopter records a change made in Fleet as the baseline of the
// identity provider's stored desired state (store.AdoptFleetAccessChange),
// guarded by the provider state read before the change
// (store.ProviderStateToken) so a provider push that lands in between wins.
type BaselineAdopter interface {
	ProviderStateToken(ctx context.Context, email string) (string, error)
	AdoptFleetAccessChange(ctx context.Context, email, token string, exists bool, chatRole, opsRole string) (bool, error)
}

// Recorder turns before/after snapshots into outbox rows. A nil *Recorder is
// the feature switched off: every method is a no-op, so callers never branch
// on configuration.
type Recorder struct {
	chat     ChatReader
	ops      OpsRoleReader
	queue    Queue
	baseline BaselineAdopter
}

// NewRecorder wires a Recorder. ops may be nil (no Operations Center plane);
// every account then reports ops_role "none". When the queue can also adopt a
// Fleet-side change into the identity provider's stored desired state (the
// chat store can), Commit does so — see Commit.
func NewRecorder(chat ChatReader, ops OpsRoleReader, queue Queue) *Recorder {
	baseline, _ := queue.(BaselineAdopter)
	return &Recorder{chat: chat, ops: ops, queue: queue, baseline: baseline}
}

// Snapshot reads email's current state across both planes. The Ops plane is
// read even when there is no Chat account: a Chat deletion whose best-effort
// Ops removal failed leaves an enabled Ops identity behind, and a feed that
// called that a deletion would tell a receiver access was fully revoked while
// it is not (see residualOps).
func (r *Recorder) Snapshot(ctx context.Context, email string) (State, error) {
	if r == nil {
		return State{}, nil
	}
	access, found, err := r.chat.AccountAccess(ctx, email)
	if err != nil {
		return State{}, fmt.Errorf("read chat account: %w", err)
	}
	opsRole := "none"
	if r.ops != nil {
		if opsRole, err = r.ops.OpsRole(ctx, email); err != nil {
			return State{}, fmt.Errorf("read ops role: %w", err)
		}
	}
	if !found {
		return State{OpsRole: opsRole}, nil
	}
	return State{Exists: true, Enabled: access.Enabled, ChatRole: access.Role, OpsRole: opsRole}, nil
}

// residualOps reports a state with no Chat account but an enabled Operations
// Center identity: reported as user.access_changed with enabled false, an
// empty chat_role and the ops_role still held — never as user.deleted, which
// promises there is no access left.
func residualOps(st State) bool {
	return !st.Exists && st.OpsRole != "" && st.OpsRole != "none"
}

// Change is an operation in progress: the state captured before it ran.
type Change struct {
	r      *Recorder
	email  string
	before State
	err    error
	// token is the provider's stored state before the change; tokenErr, when
	// set, means it could not be read and the adoption is skipped (a guess
	// could overwrite a provider push).
	token    string
	tokenErr error
}

// Begin snapshots email before a change. A failed snapshot is carried to
// Commit, which then refuses to guess and reports it (an event computed from an
// unknown "before" could be a false removal or a missed change).
func (r *Recorder) Begin(ctx context.Context, email string) *Change {
	if r == nil {
		return nil
	}
	before, err := r.Snapshot(ctx, email)
	c := &Change{r: r, email: email, before: before, err: err}
	if r.baseline != nil {
		c.token, c.tokenErr = r.baseline.ProviderStateToken(ctx, email)
	}
	return c
}

// ErrNotQueued wraps every reason Commit could not queue an event, so callers
// can tell the operator that `fleet account-events resync` will repair it.
var ErrNotQueued = errors.New("account event not queued")

// Commit reads email back after the change and queues one event when the
// resulting state differs from the snapshot. It never undoes or fails the change
// itself: callers log (HTTP) or print (CLI) a returned error and carry on.
//
// A change Fleet made itself (any source but identity_provider) is also
// adopted as the baseline of the provider's stored desired state for the
// email. That row is what the provisioning push reconciles the Ops plane from
// — even for a version it has already applied, so a retried push can finish a
// failed Ops write — and without the adoption a provider's ordinary
// redelivery of that already-applied version would silently put back the Ops
// role Fleet's admin just changed, and publish the revert tagged
// identity_provider, the one source a provider is told it may ignore. A
// change applied on the provider's word is not adopted: the push wrote the
// provider's desired state itself, and overwriting it with a half-applied
// read-back would lose a retry's target.
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
	if !after.Exists && !c.before.Exists {
		// Created and deleted inside one operation, or never a Chat account.
		return nil
	}
	typ := store.AccountEventAccessChanged
	switch {
	case residualOps(after):
		log.Printf("account events: %s has no Chat account but still holds Operations Center role %q; published as access_changed, not deleted — remove the Ops identity, then run `fleet account-events resync`",
			logSafe(c.email), after.OpsRole)
	case !after.Exists:
		typ = store.AccountEventDeleted
	}
	err = c.r.enqueue(ctx, c.email, after, typ, source, actor)
	if c.r.baseline != nil && source != store.AccountEventSourceIdentityProvider {
		adoptErr := c.tokenErr
		if adoptErr == nil {
			// adopted=false is a provider push that landed meanwhile (the more
			// recent change), or no provider row: nothing to adopt either way.
			_, adoptErr = c.r.baseline.AdoptFleetAccessChange(ctx, c.email, c.token, after.Exists, after.ChatRole, after.OpsRole)
		}
		if adoptErr != nil {
			err = errors.Join(err, fmt.Errorf("adopt the change for %s into the identity provider's desired state: %w", c.email, adoptErr))
		}
	}
	return err
}

// CommitLogged is Commit for request paths: a failure is logged, not returned.
// The message carries an email a request supplied, so line breaks are stripped
// before it reaches the log (a forged second log line is otherwise one
// "\n" away).
func (c *Change) CommitLogged(ctx context.Context, source, actor string) {
	if err := c.Commit(ctx, source, actor); err != nil {
		log.Printf("account events: %s (run `fleet account-events resync` to republish)", logSafe(err.Error()))
	}
}

func logSafe(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

// Publish queues email's current state unconditionally (resync). Accounts that
// no longer exist are skipped; PublishDeleted covers those.
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

// PublishDeleted queues a user.deleted event for email when it has no Chat
// account now (resync of a deletion whose event was lost). An email that
// exists is skipped: Publish reports it. One whose Ops identity outlived the
// Chat account is published as that residual access, not as a deletion.
func (r *Recorder) PublishDeleted(ctx context.Context, email, source, actor string) (bool, error) {
	if r == nil {
		return false, nil
	}
	st, err := r.Snapshot(ctx, email)
	if err != nil {
		return false, err
	}
	if st.Exists {
		return false, nil
	}
	typ := store.AccountEventDeleted
	if residualOps(st) {
		typ = store.AccountEventAccessChanged
	}
	return true, r.enqueue(ctx, email, st, typ, source, actor)
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
