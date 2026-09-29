package accountevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Delivery policy. The give-up horizon matches Central Auth's own back-channel
// cap so a receiver that is down for a long weekend still converges.
const (
	claimBatch = 20
	// claimLease outlives a whole batch of timed-out requests, so a row is not
	// reclaimed while this pass is still working through the batch.
	claimLease     = claimBatch * requestTimeout * 2
	pollInterval   = 2 * time.Second
	requestTimeout = 10 * time.Second
	firstRetry     = 5 * time.Second
	maxRetry       = time.Hour
	giveUpAfter    = 7 * 24 * time.Hour
	keepDelivered  = 7 * 24 * time.Hour
	keepFailed     = 30 * 24 * time.Hour
	pruneEvery     = time.Hour
)

// EventIDHeader repeats the body's id for receivers that log or dedupe before
// parsing. It is a convenience: the signed body is the authority.
const EventIDHeader = "X-Fleet-Event-Id"

// Outbox is the durable queue the Deliverer drains.
type Outbox interface {
	ClaimDueAccountEvents(ctx context.Context, now int64, limit int, lease time.Duration) ([]store.AccountEvent, error)
	MarkAccountEventDelivered(ctx context.Context, id, now int64) error
	MarkAccountEventFailed(ctx context.Context, id, now, retryAt int64, giveUp bool, message string) error
	ReleaseAccountEventLeases(ctx context.Context, ids []int64) error
	PruneAccountEvents(ctx context.Context, deliveredBefore, failedBefore int64) (int64, error)
}

// cleanupTimeout bounds the bookkeeping a pass does after its context ended:
// recording the attempt it was in the middle of, or handing unsent rows back.
const cleanupTimeout = 5 * time.Second

// Deliverer POSTs queued events to the configured URL, signed with the
// docs/WEBHOOK-SIGNING.md scheme.
type Deliverer struct {
	outbox    Outbox
	url       string
	secret    string
	client    *http.Client
	lastPrune time.Time
}

// NewDeliverer builds a Deliverer. The URL is operator-trusted (same class as
// FLEET_WEBHOOK_URL), so there is no SSRF guard and loopback is allowed. The
// client never follows redirects: a signed body must reach only the URL the
// operator named, and a 3xx is retried like any other non-2xx.
//
// An empty url is the feed switched off: the Deliverer then only runs the
// retention sweep, so rows queued while the feed was on (emails, actors,
// failure details) still age out on the documented schedule instead of
// staying forever because nothing drains the table any more.
func NewDeliverer(outbox Outbox, url, secret string) *Deliverer {
	return &Deliverer{
		outbox: outbox, url: url, secret: secret,
		client: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Payload is the wire body (docs/ACCOUNT-EVENTS.md). Field order is the
// documented order; encoding/json keeps it.
type Payload struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	OccurredAt int64       `json:"occurred_at"`
	Sequence   int64       `json:"sequence"`
	Source     string      `json:"source"`
	Actor      string      `json:"actor"`
	User       PayloadUser `json:"user"`
}

// PayloadUser is one account's resulting state; `fleet account-events export`
// prints the same shape.
type PayloadUser struct {
	Email    string `json:"email"`
	Enabled  bool   `json:"enabled"`
	ChatRole string `json:"chat_role"`
	OpsRole  string `json:"ops_role"`
}

// Body renders ev as the exact bytes a delivery sends.
func Body(ev store.AccountEvent) ([]byte, error) {
	return json.Marshal(Payload{
		ID: ev.EventID, Type: ev.Type, OccurredAt: ev.OccurredAt, Sequence: ev.ID,
		Source: ev.Source, Actor: ev.Actor,
		User: PayloadUser{Email: ev.Email, Enabled: ev.Enabled, ChatRole: ev.ChatRole, OpsRole: ev.OpsRole},
	})
}

// Run drains the outbox until ctx ends.
func (d *Deliverer) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if err := d.RunOnce(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("account events: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce delivers every event due at now (one per account per pass) and
// prunes old rows at most once per pruneEvery.
func (d *Deliverer) RunOnce(ctx context.Context, now time.Time) error {
	if now.Sub(d.lastPrune) >= pruneEvery {
		if _, err := d.outbox.PruneAccountEvents(ctx, now.Add(-keepDelivered).Unix(), now.Add(-keepFailed).Unix()); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
		d.lastPrune = now
	}
	if d.url == "" {
		return nil
	}
	events, err := d.outbox.ClaimDueAccountEvents(ctx, now.Unix(), claimBatch, claimLease)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	for i, ev := range events {
		if ctx.Err() != nil {
			// Stopped mid-batch (shutdown): hand the unsent rows back now rather
			// than leave them leased — unclaimable after a restart — for the
			// whole claimLease. ctx is already done, so the release runs on a
			// short detached one.
			return d.release(ctx, events[i:])
		}
		sendErr := d.send(ctx, ev)
		done := time.Now()
		if sendErr != nil && ctx.Err() != nil {
			// The send was cut off by the stop, not answered by the receiver:
			// not an attempt to record against the row.
			return d.release(ctx, events[i:])
		}
		if sendErr == nil {
			// The receiver has it: record that even if a stop lands now, or the
			// row is redelivered after its lease (harmless, receivers dedupe on
			// id, but avoidable).
			if err := d.book(ctx, func(c context.Context) error { return d.outbox.MarkAccountEventDelivered(c, ev.ID, done.Unix()) }); err != nil {
				return fmt.Errorf("mark delivered: %w", err)
			}
			continue
		}
		rejected := errors.Is(sendErr, errRejected)
		giveUp := rejected || done.Sub(time.Unix(ev.CreatedAt, 0)) >= giveUpAfter
		retryAt := done.Add(RetryDelay(ev.Attempts)).Unix()
		if err := d.book(ctx, func(c context.Context) error {
			return d.outbox.MarkAccountEventFailed(c, ev.ID, done.Unix(), retryAt, giveUp, sendErr.Error())
		}); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		switch {
		case rejected:
			log.Printf("account events: the receiver rejected event %d (%v); not retrying it, so the account's next event proceeds", ev.ID, sendErr)
		case giveUp:
			log.Printf("account events: gave up on event %d after %d attempts: %v", ev.ID, ev.Attempts, sendErr)
		}
	}
	return nil
}

// book runs one outbox write on a short context detached from ctx's
// cancellation, so the result of an attempt that already happened is recorded
// even when a stop arrives mid-write.
func (d *Deliverer) book(ctx context.Context, write func(context.Context) error) error {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return write(c)
}

// release hands events back to the outbox after ctx ended.
func (d *Deliverer) release(ctx context.Context, events []store.AccountEvent) error {
	ids := make([]int64, len(events))
	for i, ev := range events {
		ids[i] = ev.ID
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := d.outbox.ReleaseAccountEventLeases(cleanup, ids); err != nil {
		return fmt.Errorf("release leases: %w", err)
	}
	return ctx.Err()
}

// errRejected marks a receiver answer that retrying the same bytes cannot
// change: the body itself was refused (400 Bad Request, 409 Conflict, 410
// Gone, 413 Content Too Large, 415 Unsupported Media Type, 422 Unprocessable
// Content). Delivery is strictly in order per account, so retrying such a row
// for the whole give-up horizon would hold every later change to that account
// behind it for 7 days; giving up on it at once lets the next event — which
// carries the account's full state, not a delta — through. Everything else
// keeps retrying: a 401/403/404 is usually a secret or URL the operator can
// fix, a 408/429/5xx is the receiver having a bad moment.
var errRejected = errors.New("receiver rejected the event")

func rejectedStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusConflict, http.StatusGone,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// RetryDelay is the wait after the attempts-th failed attempt: 5s doubling to
// a one-hour ceiling.
func RetryDelay(attempts int) time.Duration {
	delay := firstRetry
	for i := 1; i < attempts && delay < maxRetry; i++ {
		delay *= 2
	}
	return min(delay, maxRetry)
}

func (d *Deliverer) send(ctx context.Context, ev store.AccountEvent) error {
	body, err := Body(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("account events request: invalid url")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventIDHeader, ev.EventID)
	notify.SignRequest(req, body, d.secret)
	resp, err := d.client.Do(req)
	if err != nil {
		return notify.SanitizeTransportError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if rejectedStatus(resp.StatusCode) {
		return fmt.Errorf("%w: status %d", errRejected, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("receiver returned status %d", resp.StatusCode)
	}
	return nil
}
