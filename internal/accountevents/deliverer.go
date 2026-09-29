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
	PruneAccountEvents(ctx context.Context, deliveredBefore, failedBefore int64) (int64, error)
}

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
	events, err := d.outbox.ClaimDueAccountEvents(ctx, now.Unix(), claimBatch, claimLease)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	for _, ev := range events {
		sendErr := d.send(ctx, ev)
		done := time.Now()
		if sendErr == nil {
			if err := d.outbox.MarkAccountEventDelivered(ctx, ev.ID, done.Unix()); err != nil {
				return fmt.Errorf("mark delivered: %w", err)
			}
			continue
		}
		giveUp := done.Sub(time.Unix(ev.CreatedAt, 0)) >= giveUpAfter
		retryAt := done.Add(RetryDelay(ev.Attempts)).Unix()
		if err := d.outbox.MarkAccountEventFailed(ctx, ev.ID, done.Unix(), retryAt, giveUp, sendErr.Error()); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		if giveUp {
			log.Printf("account events: gave up on event %d after %d attempts: %v", ev.ID, ev.Attempts, sendErr)
		}
	}
	return nil
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("receiver returned status %d", resp.StatusCode)
	}
	return nil
}
