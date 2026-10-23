// Package ownerdigest sends the daily owner digest (docs/OWNER-DIGEST.md):
// once a day, at or after an admin-chosen UTC hour, one email to each task
// owner listing that owner's scheduled tasks that are not working — a latest
// run that was blocked, failed (dead-lettered or errored) or stopped its
// schedule, and occurrences that are overdue (due more than two hours ago and
// not started). An owner with nothing to report gets nothing.
//
// It is deterministic Go over the sched store: no model is involved in
// choosing, wording or sending anything.
//
// Where it reads and sends:
//
//   - The settings (on/off, hour) and the SMTP settings come from the ONE
//     shared *notify.Notifier, read fresh on every tick, so an admin edit in
//     Settings → Admin → Notifications takes effect on the next tick without a
//     restart (the same hot-swap every other notification uses).
//   - The email goes to the owner, resolved from the task's created_by exactly
//     as the runner resolves a notification's Audience (the sched username is
//     the account email); the deployment-wide recipient list is not used. It
//     needs only an SMTP host and a From address. Without them the day's
//     digest is a logged no-op, re-checked every tick so configuring SMTP later
//     the same day still sends it.
//   - Once a day per deployment: the day is claimed in the chat store
//     (owner_digest_days, migration 083) before anything is sent, and only the
//     claim that inserted the row sends. A restart, a second replica or a
//     later tick finds the day claimed. A crash between the claim and the last
//     send loses (part of) that day's digest rather than sending it twice.
package ownerdigest

import (
	"context"
	"fmt"
	"log"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/truncate"
)

const (
	// Window is how far back a finished run counts: the digest lists problems
	// from the last 24 hours.
	Window = 24 * time.Hour
	// OverdueAfter is how long past its scheduled time a waiting occurrence
	// must be before the digest calls it overdue: long enough that a busy
	// queue is not reported, short enough that a stuck schedule is.
	OverdueAfter = 2 * time.Hour

	// candidateLimit caps each candidate query (db.OwnerDigestCandidates);
	// a deployment past it has bigger problems than a digest can describe.
	candidateLimit = 5000
	// maxItemsPerOwner caps one email; the rest are counted, not listed.
	maxItemsPerOwner = 50
	// maxReasonBytes bounds each reason line (a dead-letter reason can quote a
	// long provider error).
	maxReasonBytes = 400
	// maxTitleBytes bounds an untitled task's prompt-derived label.
	maxTitleBytes = 100

	// tickInterval is how often the loop checks whether the day's digest is
	// due. The check reads only the in-memory config until it is.
	tickInterval = time.Minute
	// sendBudget bounds one day's whole send, so a dead SMTP server cannot
	// hold the loop for hours (each send already has notify's timeout and
	// bounded retry).
	sendBudget = 15 * time.Minute
)

// TaskStore is the sched-store seam (satisfied by *storage.Storage).
type TaskStore interface {
	OwnerDigestCandidates(ctx context.Context, since, overdueBefore time.Time, limit int) ([]*models.Task, error)
	// GetUsersByIDsWithContext resolves created_by to the account email —
	// the lookup the runner uses for a notification's Audience.
	GetUsersByIDsWithContext(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Claimer is the once-a-day claim (satisfied by *store.Store).
type Claimer interface {
	ClaimOwnerDigestDay(ctx context.Context, day time.Time) (bool, error)
}

// Mailer is the notifier seam (satisfied by *notify.Notifier).
type Mailer interface {
	Config() notify.Config
	SendDirectEmail(ctx context.Context, to, subject, textBody, htmlBody string) error
}

// Kind is what is wrong with one listed task.
type Kind string

const (
	// KindStopped is a dead-lettered occurrence that parked its recurring
	// schedule (ADR-0070): nothing will run again until someone acts.
	KindStopped Kind = "stopped"
	// KindFailed is a latest run that dead-lettered or errored.
	KindFailed Kind = "failed"
	// KindBlocked is a latest run that finished but recorded a blocked
	// outcome (ADR-0092): what it maintains was not updated.
	KindBlocked Kind = "blocked"
	// KindOverdue is an occurrence due more than OverdueAfter ago that has not
	// started.
	KindOverdue Kind = "overdue"
)

// kindOrder is the order sections appear in: the most severe first.
var kindOrder = map[Kind]int{KindStopped: 0, KindFailed: 1, KindBlocked: 2, KindOverdue: 3}

// Item is one listed problem.
type Item struct {
	Kind   Kind
	TaskID uuid.UUID
	Title  string
	// When is when the run finished (or, for KindOverdue, when it was due).
	When time.Time
	// Reason says why, in the run's own recorded words where there are any.
	Reason string
	// Detail is an optional second line (a stopped schedule's last failure,
	// an overdue occurrence's run_if note).
	Detail string
	// LogURL opens the task in the Operations Center, built exactly as a
	// notification's Event.LogURL; "" when no public URL is configured.
	LogURL string
}

// Digest is one owner's email.
type Digest struct {
	Owner string
	Items []Item
	// Omitted counts problems past maxItemsPerOwner, which are not listed.
	Omitted int
}

// Classify returns the problem a candidate row reports at now, or ok=false
// when it reports none (a healthy run, a run outside the window, an
// occurrence not yet overdue, an ownerless task). publicURLBase builds the
// log link (FLEET_PUBLIC_URL, "" = no link).
func Classify(t *models.Task, now time.Time, publicURLBase string) (Item, bool) {
	if t == nil || t.CreatedBy == nil {
		return Item{}, false
	}
	it := Item{TaskID: t.ID, Title: taskTitle(t), LogURL: logURL(publicURLBase, t.ID)}
	scheduled := t.Recurrence != "" || t.ScheduledFor != nil
	finished := finishedAt(t)
	inWindow := finished != nil && !finished.Before(now.Add(-Window))
	switch t.Status {
	case models.TaskStatusSuccess:
		if !t.IsRunOutcome(models.RunOutcomeBlocked) || !scheduled || !inWindow {
			return Item{}, false
		}
		it.Kind, it.When = KindBlocked, *finished
		it.Reason = clean(deref(t.RunOutcomeDetail), "no reason was recorded")
	case models.TaskStatusDeadLettered, models.TaskStatusError:
		if !scheduled || !inWindow {
			return Item{}, false
		}
		it.When = *finished
		failure := clean(firstNonEmpty(deref(t.DeadLetterReason), deref(t.ErrorMessage)), "no reason was recorded")
		if t.Recurrence != "" && t.RecurrenceParkedAt != nil {
			it.Kind = KindStopped
			it.Reason = clean(deref(t.RecurrenceParkedReason), "the schedule was stopped after repeated failures")
			it.Detail = "Last run failed: " + failure
		} else {
			it.Kind, it.Reason = KindFailed, failure
		}
	case models.TaskStatusScheduled, models.TaskStatusPending:
		if t.ScheduledFor == nil || (t.TriggerType != "" && t.TriggerType != models.TriggerTypeCron) {
			return Item{}, false
		}
		late := now.Sub(*t.ScheduledFor)
		if late <= OverdueAfter {
			return Item{}, false
		}
		it.Kind, it.When = KindOverdue, *t.ScheduledFor
		it.Reason = fmt.Sprintf("Due %s ago and not started (it is still %s).", humanDuration(late), t.Status)
		if t.RunIf != nil {
			it.Detail = "It has a run_if check, which may be holding it."
		}
	default:
		return Item{}, false
	}
	return it, true
}

// Build groups candidate rows into one digest per owner email, the owners
// sorted by address and each owner's items by severity then time. owners maps
// created_by to the account email (GetUsersByIDsWithContext); a task whose
// owner has no address that can receive mail is skipped (counted in
// skipped). An owner with no problems gets no digest.
func Build(tasks []*models.Task, owners map[uuid.UUID]string, now time.Time, publicURLBase string) (digests []Digest, skipped int) {
	byOwner := map[string][]Item{}
	seen := map[uuid.UUID]bool{}
	for _, t := range tasks {
		if t == nil || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		it, ok := Classify(t, now, publicURLBase)
		if !ok {
			continue
		}
		owner, ok := mailbox(owners[*t.CreatedBy])
		if !ok {
			skipped++
			continue
		}
		byOwner[owner] = append(byOwner[owner], it)
	}
	for owner, items := range byOwner {
		sort.SliceStable(items, func(i, j int) bool {
			if kindOrder[items[i].Kind] != kindOrder[items[j].Kind] {
				return kindOrder[items[i].Kind] < kindOrder[items[j].Kind]
			}
			if !items[i].When.Equal(items[j].When) {
				return items[i].When.Before(items[j].When)
			}
			return items[i].TaskID.String() < items[j].TaskID.String()
		})
		d := Digest{Owner: owner, Items: items}
		if len(items) > maxItemsPerOwner {
			d.Items, d.Omitted = items[:maxItemsPerOwner], len(items)-maxItemsPerOwner
		}
		digests = append(digests, d)
	}
	sort.Slice(digests, func(i, j int) bool { return digests[i].Owner < digests[j].Owner })
	return digests, skipped
}

// Outcome names what one Tick did.
type Outcome string

const (
	OutcomeDisabled      Outcome = "disabled"          // the digest is off
	OutcomeNotYet        Outcome = "not_yet"           // before today's hour
	OutcomeDone          Outcome = "done"              // this process already handled today
	OutcomeNoSMTP        Outcome = "no_smtp"           // SMTP host/From not configured: logged no-op
	OutcomeError         Outcome = "error"             // a read or the claim failed; the next tick retries
	OutcomeClaimedBefore Outcome = "claimed_elsewhere" // another process, or this one before a restart, sent today's
	OutcomeSent          Outcome = "sent"              // this tick claimed today and sent
)

// TickResult is what one Tick did (for tests and the log line).
type TickResult struct {
	Outcome Outcome
	Owners  int // digests built (owners with something to report)
	Sent    int
	Failed  int
	Skipped int // problems whose owner has no deliverable address
}

// Service runs the daily digest.
type Service struct {
	tasks  TaskStore
	claims Claimer
	mailer Mailer
	now    func() time.Time
	logf   func(format string, args ...any)

	mu sync.Mutex
	// doneDay is the UTC day (YYYY-MM-DD) this process has settled: it
	// claimed and sent, or found the day claimed. Saves a query per tick for
	// the rest of the day; the persisted claim is what makes it once a day.
	doneDay string
	// noSMTPDay is the UTC day the not-configured no-op was last logged, so
	// the log says it once a day rather than every tick.
	noSMTPDay string
}

// New builds the service.
func New(tasks TaskStore, claims Claimer, mailer Mailer) *Service {
	return &Service{
		tasks: tasks, claims: claims, mailer: mailer,
		now:  time.Now,
		logf: log.Printf,
	}
}

// Start runs Tick every tickInterval until ctx ends. A panic in a tick is
// contained; the next tick runs.
func (s *Service) Start(ctx context.Context) {
	safe.Go("ownerdigest.loop", func() {
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				func() {
					defer safe.Recover("ownerdigest.tick", nil)
					s.Tick(ctx)
				}()
			}
		}
	})
}

// Tick sends today's digest if it is due and nobody has sent it yet. Exported
// so tests drive one pass without the ticker.
func (s *Service) Tick(ctx context.Context) TickResult {
	cfg := s.mailer.Config()
	if !cfg.OwnerDigestEnabled {
		return TickResult{Outcome: OutcomeDisabled}
	}
	now := s.now().UTC()
	if now.Hour() < cfg.OwnerDigestHourUTC {
		return TickResult{Outcome: OutcomeNotYet}
	}
	day := now.Format(time.DateOnly)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doneDay == day {
		return TickResult{Outcome: OutcomeDone}
	}
	if !cfg.DirectEmailConfigured() {
		if s.noSMTPDay != day {
			s.noSMTPDay = day
			s.logf("owner digest: not sent for %s — SMTP is not configured (an SMTP host and a From address are needed; Settings → Admin → Notifications)", day)
		}
		return TickResult{Outcome: OutcomeNoSMTP}
	}

	// Read before claiming, so a failed read leaves the day unclaimed and the
	// next tick retries instead of losing it.
	digests, skipped, err := s.collect(ctx, now, cfg.PublicURLBase)
	if err != nil {
		s.logf("owner digest: reading tasks for %s failed: %v (retrying next tick)", day, err)
		return TickResult{Outcome: OutcomeError}
	}
	won, err := s.claims.ClaimOwnerDigestDay(ctx, now)
	if err != nil {
		s.logf("owner digest: claiming %s failed: %v (retrying next tick)", day, err)
		return TickResult{Outcome: OutcomeError}
	}
	s.doneDay = day
	if !won {
		return TickResult{Outcome: OutcomeClaimedBefore}
	}

	res := TickResult{Outcome: OutcomeSent, Owners: len(digests), Skipped: skipped}
	sendCtx, cancel := context.WithTimeout(ctx, sendBudget)
	defer cancel()
	for _, d := range digests {
		subject, text, html := Render(d, now)
		if err := s.mailer.SendDirectEmail(sendCtx, d.Owner, subject, text, html); err != nil {
			res.Failed++
			s.logf("owner digest: sending to %s failed: %v", d.Owner, err)
			continue
		}
		res.Sent++
	}
	s.logf("owner digest: %s — sent %d of %d owner digest(s), %d failed; %d problem(s) skipped for an owner without an email address",
		day, res.Sent, res.Owners, res.Failed, res.Skipped)
	return res
}

// collect reads the candidates, resolves their owners and builds the digests.
func (s *Service) collect(ctx context.Context, now time.Time, publicURLBase string) ([]Digest, int, error) {
	tasks, err := s.tasks.OwnerDigestCandidates(ctx, now.Add(-Window), now.Add(-OverdueAfter), candidateLimit)
	if err != nil {
		return nil, 0, err
	}
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, t := range tasks {
		if t != nil && t.CreatedBy != nil && !seen[*t.CreatedBy] {
			seen[*t.CreatedBy] = true
			ids = append(ids, *t.CreatedBy)
		}
	}
	owners := map[uuid.UUID]string{}
	if len(ids) > 0 {
		if owners, err = s.tasks.GetUsersByIDsWithContext(ctx, ids); err != nil {
			return nil, 0, fmt.Errorf("resolve owners: %w", err)
		}
	}
	digests, skipped := Build(tasks, owners, now, publicURLBase)
	return digests, skipped, nil
}

// finishedAt is when a terminal run finished: completed_at, which a
// dead-letter stamps too, else dead_lettered_at.
func finishedAt(t *models.Task) *time.Time {
	if t.CompletedAt != nil {
		return t.CompletedAt
	}
	return t.DeadLetteredAt
}

// taskTitle is the task's own title, or its prompt on one line, bounded.
func taskTitle(t *models.Task) string {
	if title := strings.Join(strings.Fields(t.Title), " "); title != "" {
		return truncate.Clamp(title, maxTitleBytes, "…")
	}
	if p := strings.Join(strings.Fields(t.Prompt), " "); p != "" {
		return truncate.Clamp(p, maxTitleBytes, "…")
	}
	return "Untitled task"
}

// logURL is the Operations Center link, built as the runner builds
// Event.LogURL.
func logURL(base string, id uuid.UUID) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return base + "/orchestrator/tasks/" + id.String()
}

// mailbox returns the owner's address when it is a bare, deliverable one
// (the sched username is the account email on a standard deployment; a local
// username such as "admin" cannot be mailed).
func mailbox(username string) (string, bool) {
	u := strings.TrimSpace(username)
	if u == "" || strings.ContainsAny(u, "\r\n<>, ") {
		return "", false
	}
	addr, err := mail.ParseAddress(u)
	if err != nil || addr.Address != u || addr.Name != "" {
		return "", false
	}
	return u, true
}

func clean(s, fallback string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return fallback
	}
	return truncate.Clamp(s, maxReasonBytes, "…")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// humanDuration renders a lateness as "3h 10m" / "2d 4h".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
