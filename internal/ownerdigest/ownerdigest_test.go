package ownerdigest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

var (
	testNow = time.Date(2026, 10, 23, 13, 4, 0, 0, time.UTC)
	ownerA  = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	ownerB  = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	noMail  = uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
)

func ptr[T any](v T) *T { return &v }

func ago(d time.Duration) *time.Time { return ptr(testNow.Add(-d)) }

func task(title string, owner *uuid.UUID, status models.TaskStatus, mut func(*models.Task)) *models.Task {
	t := &models.Task{ID: uuid.New(), Title: title, Prompt: "prompt of " + title, Status: status, CreatedBy: owner, Recurrence: "0 6 * * *", TriggerType: models.TriggerTypeCron}
	if mut != nil {
		mut(t)
	}
	return t
}

// TestClassify pins which rows are problems and how each is worded.
func TestClassify(t *testing.T) {
	base := "https://fleet.example.com/"
	cases := []struct {
		name   string
		task   *models.Task
		want   Kind // "" = not reported
		reason string
		detail string
	}{
		{"blocked latest run", task("Acme refresh", &ownerA, models.TaskStatusSuccess, func(t *models.Task) {
			t.RunOutcome, t.RunOutcomeDetail, t.CompletedAt = ptr(models.RunOutcomeBlocked), ptr("outcome=source_unreachable:\n mailbox empty"), ago(3*time.Hour)
		}), KindBlocked, "outcome=source_unreachable: mailbox empty", ""},
		{"blocked without a recorded reason", task("x", &ownerA, models.TaskStatusSuccess, func(t *models.Task) {
			t.RunOutcome, t.CompletedAt = ptr(models.RunOutcomeBlocked), ago(time.Hour)
		}), KindBlocked, "no reason was recorded", ""},
		{"healthy success", task("ok", &ownerA, models.TaskStatusSuccess, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }), "", "", ""},
		{"dead-lettered", task("dl", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
			t.CompletedAt, t.DeadLetterReason, t.ErrorMessage = ago(2*time.Hour), ptr("non-retryable failure (terminal): bad key"), ptr("raw")
		}), KindFailed, "non-retryable failure (terminal): bad key", ""},
		{"errored one-off scheduled run", task("err", &ownerA, models.TaskStatusError, func(t *models.Task) {
			t.Recurrence, t.ScheduledFor, t.CompletedAt, t.ErrorMessage = "", ago(5*time.Hour), ago(4*time.Hour), ptr("expired: awaited input")
		}), KindFailed, "expired: awaited input", ""},
		{"dead-letter that parked the schedule", task("parked", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
			t.CompletedAt, t.DeadLetterReason = ago(time.Hour), ptr("boom")
			t.RecurrenceParkedAt, t.RecurrenceParkedReason = ago(time.Hour), ptr("Two consecutive occurrences were dead-lettered.")
		}), KindStopped, "Two consecutive occurrences were dead-lettered.", "Last run failed: boom"},
		{"parked without a reason", task("parked2", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
			t.CompletedAt, t.RecurrenceParkedAt = ago(time.Hour), ago(time.Hour)
		}), KindStopped, "the schedule was stopped after repeated failures", "Last run failed: no reason was recorded"},
		{"failure outside the window", task("old", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(25 * time.Hour) }), "", "", ""},
		{"run on the spot (not scheduled)", task("adhoc", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
			t.Recurrence, t.CompletedAt = "", ago(time.Hour)
		}), "", "", ""},
		{"ownerless", task("nobody", nil, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }), "", "", ""},
		{"cancelled", task("stop", &ownerA, models.TaskStatusCancelled, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }), "", "", ""},
		{"overdue occurrence", task("late", &ownerA, models.TaskStatusScheduled, func(t *models.Task) { t.ScheduledFor = ago(3*time.Hour + 10*time.Minute) }),
			KindOverdue, "Due 3h 10m ago and not started (it is still scheduled).", ""},
		{"overdue pending with run_if", task("gated", &ownerA, models.TaskStatusPending, func(t *models.Task) {
			t.ScheduledFor, t.RunIf = ago(50*time.Hour), &models.RunIf{}
		}), KindOverdue, "Due 2d 2h ago and not started (it is still pending).", "It has a run_if check, which may be holding it."},
		{"due, but within two hours", task("soon", &ownerA, models.TaskStatusScheduled, func(t *models.Task) { t.ScheduledFor = ago(2 * time.Hour) }), "", "", ""},
		{"webhook template", task("hook", &ownerA, models.TaskStatusScheduled, func(t *models.Task) {
			t.ScheduledFor, t.TriggerType = ago(5*time.Hour), models.TriggerTypeWebhook
		}), "", "", ""},
		{"running", task("busy", &ownerA, models.TaskStatusRunning, func(t *models.Task) { t.ScheduledFor = ago(5 * time.Hour) }), "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it, ok := Classify(tc.task, testNow, base)
			if tc.want == "" {
				if ok {
					t.Fatalf("reported %+v, want nothing", it)
				}
				return
			}
			if !ok || it.Kind != tc.want || it.Reason != tc.reason || it.Detail != tc.detail {
				t.Fatalf("got ok=%v %+v, want %s / %q / %q", ok, it, tc.want, tc.reason, tc.detail)
			}
			if it.LogURL != "https://fleet.example.com/orchestrator/tasks/"+tc.task.ID.String() {
				t.Errorf("log url = %q", it.LogURL)
			}
		})
	}
}

// TestBuildGroupsPerOwner: one digest per owner with problems, sorted by
// severity; a healthy owner gets nothing; ownerless rows and owners with no
// deliverable address are skipped (the latter counted).
func TestBuildGroupsPerOwner(t *testing.T) {
	blocked := task("B blocked", &ownerA, models.TaskStatusSuccess, func(t *models.Task) {
		t.RunOutcome, t.RunOutcomeDetail, t.CompletedAt = ptr(models.RunOutcomeBlocked), ptr("no data"), ago(2*time.Hour)
	})
	stopped := task("A stopped", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
		t.CompletedAt, t.RecurrenceParkedAt = ago(5*time.Hour), ago(5*time.Hour)
	})
	overdue := task("C late", &ownerA, models.TaskStatusScheduled, func(t *models.Task) { t.ScheduledFor = ago(3 * time.Hour) })
	healthyB := task("fine", &ownerB, models.TaskStatusSuccess, func(t *models.Task) { t.CompletedAt = ago(time.Hour) })
	ownerless := task("orphan", nil, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(time.Hour) })
	localUser := task("local", &noMail, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(time.Hour) })

	owners := map[uuid.UUID]string{ownerA: "alice@example.com", ownerB: "bob@example.com", noMail: "admin"}
	digests, skipped := Build([]*models.Task{overdue, blocked, healthyB, stopped, ownerless, localUser, blocked}, owners, testNow, "")
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the local username)", skipped)
	}
	if len(digests) != 1 || digests[0].Owner != "alice@example.com" {
		t.Fatalf("digests = %+v, want Alice's only", digests)
	}
	kinds := make([]Kind, 0, len(digests[0].Items))
	for _, it := range digests[0].Items {
		kinds = append(kinds, it.Kind)
		if it.LogURL != "" {
			t.Errorf("no public URL configured, but item has link %q", it.LogURL)
		}
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]Kind{KindStopped, KindBlocked, KindOverdue}) {
		t.Errorf("items = %v, want stopped, blocked, overdue (a duplicate row listed once)", kinds)
	}
}

// TestBuildCapsItemsPerOwner: past maxItemsPerOwner the rest are counted.
func TestBuildCapsItemsPerOwner(t *testing.T) {
	var tasks []*models.Task
	for i := 0; i < maxItemsPerOwner+7; i++ {
		tasks = append(tasks, task(fmt.Sprintf("t%d", i), &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }))
	}
	digests, _ := Build(tasks, map[uuid.UUID]string{ownerA: "alice@example.com"}, testNow, "")
	if len(digests) != 1 || len(digests[0].Items) != maxItemsPerOwner || digests[0].Omitted != 7 {
		t.Fatalf("got %d items, %d omitted", len(digests[0].Items), digests[0].Omitted)
	}
	subject, text, _ := Render(digests[0], testNow)
	if !strings.Contains(subject, fmt.Sprintf("%d scheduled tasks need attention", maxItemsPerOwner+7)) || !strings.Contains(text, "…and 7 more not listed here") {
		t.Errorf("render of a capped digest: %q\n%s", subject, text)
	}
}

// TestRender: sections, escaping, links and the Task ID fallback.
func TestRender(t *testing.T) {
	id := uuid.New()
	d := Digest{Owner: "alice@example.com", Items: []Item{
		{Kind: KindFailed, TaskID: id, Title: `<script>alert("x")</script>`, When: testNow.Add(-time.Hour), Reason: "a < b & c", LogURL: "https://fleet.example.com/orchestrator/tasks/" + id.String()},
		{Kind: KindBlocked, TaskID: id, Title: "Refresh", When: testNow.Add(-2 * time.Hour), Reason: "source_unreachable"},
	}}
	subject, text, html := Render(d, testNow)
	if subject != "Fleet daily digest: 2 scheduled tasks need attention" {
		t.Errorf("subject = %q", subject)
	}
	for _, want := range []string{
		"for the 24 hours up to 2026-10-23 13:04 UTC",
		"FAILED (1)", "BLOCKED (1)",
		"  Failed: 2026-10-23 12:04 UTC\r\n  Reason: a < b & c",
		"  Open: https://fleet.example.com/orchestrator/tasks/" + id.String(),
		"  Ran: 2026-10-23 11:04 UTC\r\n  Reason: source_unreachable\r\n  Task ID: " + id.String(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q\n%s", want, text)
		}
	}
	if strings.Index(text, "FAILED") > strings.Index(text, "BLOCKED") {
		t.Error("failed must come before blocked")
	}
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "a &lt; b &amp; c") {
		t.Errorf("html not escaped:\n%s", html)
	}
	if !strings.Contains(html, `<a href="https://fleet.example.com/orchestrator/tasks/`+id.String()+`">`) {
		t.Errorf("html missing the link:\n%s", html)
	}
	one, _, _ := Render(Digest{Owner: "a@b.c", Items: d.Items[:1]}, testNow)
	if one != "Fleet daily digest: 1 scheduled task needs attention" {
		t.Errorf("singular subject = %q", one)
	}
}

// ── Tick ──

type fakeTasks struct {
	mu      sync.Mutex
	tasks   []*models.Task
	owners  map[uuid.UUID]string
	err     error
	queries int
	since   time.Time
	overdue time.Time
}

func (f *fakeTasks) OwnerDigestCandidates(_ context.Context, since, overdueBefore time.Time, _ int) ([]*models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	f.since, f.overdue = since, overdueBefore
	return f.tasks, f.err
}

func (f *fakeTasks) GetUsersByIDsWithContext(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if e, ok := f.owners[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

// fakeClaims is the persisted claim table: shared between Service instances
// to model a restart or a second replica.
type fakeClaims struct {
	mu   sync.Mutex
	days map[string]bool
	err  error
}

func (f *fakeClaims) ClaimOwnerDigestDay(_ context.Context, day time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	d := day.UTC().Format(time.DateOnly)
	if f.days[d] {
		return false, nil
	}
	if f.days == nil {
		f.days = map[string]bool{}
	}
	f.days[d] = true
	return true, nil
}

type sentMail struct{ to, subject, text, html string }

type fakeMailer struct {
	mu   sync.Mutex
	cfg  notify.Config
	sent []sentMail
	fail map[string]error
}

func (f *fakeMailer) Config() notify.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeMailer) SendDirectEmail(_ context.Context, to, subject, text, html string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[to]; err != nil {
		return err
	}
	f.sent = append(f.sent, sentMail{to, subject, text, html})
	return nil
}

func (f *fakeMailer) setConfig(cfg notify.Config) {
	f.mu.Lock()
	f.cfg = cfg
	f.mu.Unlock()
}

func smtpConfig(enabled bool, hour int) notify.Config {
	return notify.Config{
		SMTPHost: "smtp.example.com", SMTPFrom: "fleet@example.com", EmailTo: []string{"ops@example.com"},
		PublicURLBase:      "https://fleet.example.com",
		OwnerDigestEnabled: enabled, OwnerDigestHourUTC: hour,
	}
}

type harness struct {
	tasks  *fakeTasks
	claims *fakeClaims
	mailer *fakeMailer
	logs   []string
	clock  time.Time
}

func newHarness() *harness {
	return &harness{
		tasks: &fakeTasks{owners: map[uuid.UUID]string{ownerA: "alice@example.com", ownerB: "bob@example.com"}, tasks: []*models.Task{
			task("Acme refresh", &ownerA, models.TaskStatusSuccess, func(t *models.Task) {
				t.RunOutcome, t.RunOutcomeDetail, t.CompletedAt = ptr(models.RunOutcomeBlocked), ptr("source_unreachable"), ago(5*time.Hour)
			}),
			task("Bob healthy", &ownerB, models.TaskStatusSuccess, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }),
		}},
		claims: &fakeClaims{},
		mailer: &fakeMailer{cfg: smtpConfig(true, 13)},
		clock:  testNow,
	}
}

// service builds a fresh Service over the shared fakes — a new one models a
// process restart (no in-memory state, the same persisted claims).
func (h *harness) service() *Service {
	s := New(h.tasks, h.claims, h.mailer)
	s.now = func() time.Time { return h.clock }
	s.logf = func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) }
	return s
}

// TestTickSendsOncePerDay: the due tick sends each owner with a problem one
// email to their own address (not the deployment list); later ticks, a
// restarted process and a second replica send nothing more that day; the
// next day sends again.
func TestTickSendsOncePerDay(t *testing.T) {
	h := newHarness()
	s := h.service()

	res := s.Tick(context.Background())
	if res.Outcome != OutcomeSent || res.Owners != 1 || res.Sent != 1 || res.Failed != 0 {
		t.Fatalf("first tick = %+v", res)
	}
	if len(h.mailer.sent) != 1 || h.mailer.sent[0].to != "alice@example.com" {
		t.Fatalf("sent = %+v, want one email to Alice (Bob is healthy)", h.mailer.sent)
	}
	m := h.mailer.sent[0]
	if !strings.Contains(m.text, "Acme refresh") || !strings.Contains(m.text, "Reason: source_unreachable") ||
		!strings.Contains(m.text, "Open: https://fleet.example.com/orchestrator/tasks/") {
		t.Errorf("digest body:\n%s", m.text)
	}
	if !h.tasks.since.Equal(testNow.Add(-24*time.Hour)) || !h.tasks.overdue.Equal(testNow.Add(-2*time.Hour)) {
		t.Errorf("window = since %v overdue %v", h.tasks.since, h.tasks.overdue)
	}

	// Same process, later the same day: settled in memory, no query.
	h.clock = testNow.Add(3 * time.Hour)
	if res := s.Tick(context.Background()); res.Outcome != OutcomeDone {
		t.Fatalf("second tick = %+v, want done", res)
	}
	if h.tasks.queries != 1 {
		t.Errorf("a settled day re-queried: %d queries", h.tasks.queries)
	}

	// A restart (or another replica) the same day finds the day claimed.
	if res := h.service().Tick(context.Background()); res.Outcome != OutcomeClaimedBefore {
		t.Fatalf("restarted tick = %+v, want claimed_elsewhere", res)
	}
	if len(h.mailer.sent) != 1 {
		t.Fatalf("sent %d emails in one day, want 1", len(h.mailer.sent))
	}

	// The next day, at the hour, sends again — yesterday's blocked run has
	// aged out of the window, so only the new failure is listed.
	h.clock = testNow.Add(24 * time.Hour)
	h.tasks.tasks = append(h.tasks.tasks, task("Acme export", &ownerA, models.TaskStatusDeadLettered, func(t *models.Task) {
		t.CompletedAt = ptr(h.clock.Add(-time.Hour))
	}))
	if res := s.Tick(context.Background()); res.Outcome != OutcomeSent || len(h.mailer.sent) != 2 {
		t.Fatalf("next day = %+v, sent %d", res, len(h.mailer.sent))
	}
	if next := h.mailer.sent[1].text; !strings.Contains(next, "Acme export") || strings.Contains(next, "Acme refresh") {
		t.Errorf("next day's digest:\n%s", next)
	}
}

// TestTickWaitsForTheHourAndTheSwitch: off sends nothing; before the hour
// sends nothing; turning it on (a hot-swapped config) at or after the hour
// sends on the next tick.
func TestTickWaitsForTheHourAndTheSwitch(t *testing.T) {
	h := newHarness()
	h.mailer.setConfig(smtpConfig(false, 13))
	s := h.service()
	if res := s.Tick(context.Background()); res.Outcome != OutcomeDisabled {
		t.Fatalf("disabled tick = %+v", res)
	}
	h.mailer.setConfig(smtpConfig(true, 14))
	if res := s.Tick(context.Background()); res.Outcome != OutcomeNotYet {
		t.Fatalf("before the hour = %+v", res)
	}
	if h.tasks.queries != 0 || len(h.claims.days) != 0 {
		t.Fatal("nothing may be read or claimed before the digest is due")
	}
	h.mailer.setConfig(smtpConfig(true, 0))
	if res := s.Tick(context.Background()); res.Outcome != OutcomeSent || len(h.mailer.sent) != 1 {
		t.Fatalf("after the switch = %+v, sent %d", res, len(h.mailer.sent))
	}
}

// TestTickWithoutSMTPIsALoggedNoOp: no SMTP host/From → nothing is read,
// claimed or sent, the log says so once a day, and configuring SMTP later the
// same day still sends that day's digest.
func TestTickWithoutSMTPIsALoggedNoOp(t *testing.T) {
	h := newHarness()
	cfg := smtpConfig(true, 13)
	cfg.SMTPHost = ""
	h.mailer.setConfig(cfg)
	s := h.service()
	for i := 0; i < 3; i++ {
		if res := s.Tick(context.Background()); res.Outcome != OutcomeNoSMTP {
			t.Fatalf("tick %d = %+v, want no_smtp", i, res)
		}
	}
	if h.tasks.queries != 0 || len(h.claims.days) != 0 || len(h.mailer.sent) != 0 {
		t.Fatalf("no-op touched something: queries=%d claims=%v sent=%d", h.tasks.queries, h.claims.days, len(h.mailer.sent))
	}
	if len(h.logs) != 1 || !strings.Contains(h.logs[0], "SMTP is not configured") {
		t.Fatalf("logs = %q, want the no-op logged once", h.logs)
	}
	h.mailer.setConfig(smtpConfig(true, 13))
	if res := s.Tick(context.Background()); res.Outcome != OutcomeSent || len(h.mailer.sent) != 1 {
		t.Fatalf("after configuring SMTP = %+v", res)
	}
}

// TestTickRetriesAReadOrClaimFailure: a failed read leaves the day unclaimed;
// a failed claim sends nothing; both retry on the next tick.
func TestTickRetriesAReadOrClaimFailure(t *testing.T) {
	h := newHarness()
	s := h.service()
	h.tasks.err = errors.New("db down")
	if res := s.Tick(context.Background()); res.Outcome != OutcomeError || len(h.claims.days) != 0 {
		t.Fatalf("read failure = %+v, claims %v", res, h.claims.days)
	}
	h.tasks.err = nil
	h.claims.err = errors.New("chat db down")
	if res := s.Tick(context.Background()); res.Outcome != OutcomeError || len(h.mailer.sent) != 0 {
		t.Fatalf("claim failure = %+v", res)
	}
	h.claims.err = nil
	if res := s.Tick(context.Background()); res.Outcome != OutcomeSent || len(h.mailer.sent) != 1 {
		t.Fatalf("retry = %+v", res)
	}
}

// TestTickCountsDeliveryFailures: one owner's failed send does not stop the
// others, and the day stays claimed (no second attempt that day).
func TestTickCountsDeliveryFailures(t *testing.T) {
	h := newHarness()
	h.tasks.tasks = append(h.tasks.tasks, task("Bob broken", &ownerB, models.TaskStatusDeadLettered, func(t *models.Task) { t.CompletedAt = ago(time.Hour) }))
	h.mailer.fail = map[string]error{"alice@example.com": errors.New("smtp 550")}
	s := h.service()
	res := s.Tick(context.Background())
	if res.Outcome != OutcomeSent || res.Owners != 2 || res.Sent != 1 || res.Failed != 1 {
		t.Fatalf("tick = %+v", res)
	}
	if len(h.mailer.sent) != 1 || h.mailer.sent[0].to != "bob@example.com" {
		t.Fatalf("sent = %+v", h.mailer.sent)
	}
	if res := h.service().Tick(context.Background()); res.Outcome != OutcomeClaimedBefore {
		t.Fatalf("after a partial failure = %+v, want the day to stay claimed", res)
	}
}

// TestTickNobodyToTell: with no problems anywhere nothing is sent, and the
// day is still settled.
func TestTickNobodyToTell(t *testing.T) {
	h := newHarness()
	h.tasks.tasks = h.tasks.tasks[1:] // Bob's healthy run only
	s := h.service()
	res := s.Tick(context.Background())
	if res.Outcome != OutcomeSent || res.Owners != 0 || len(h.mailer.sent) != 0 {
		t.Fatalf("tick = %+v, sent %d", res, len(h.mailer.sent))
	}
	if res := s.Tick(context.Background()); res.Outcome != OutcomeDone {
		t.Fatalf("second tick = %+v", res)
	}
}

// The real notifier must satisfy the seam the digest sends through.
var _ Mailer = (*notify.Notifier)(nil)
