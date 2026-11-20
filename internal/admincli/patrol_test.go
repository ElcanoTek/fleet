package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

type fakePatrolStore struct {
	sched     store.AdminChatSchedule
	edits     []store.AdminChatSchedulePromptEdit
	schedEdit []store.AdminChatScheduleCronEdit
}

// AdminSetChatScheduleCron mimics the store: compare-and-swap on
// def_version, the real cron validation and preview, and a write only when
// it is not a dry run.
func (f *fakePatrolStore) AdminSetChatScheduleCron(_ context.Context, _ store.AdminActor, in store.AdminChatScheduleCronEdit) (store.AdminChatScheduleCronResult, error) {
	res := store.AdminChatScheduleCronResult{Before: f.sched, After: f.sched, DryRun: in.DryRun}
	if !strings.HasPrefix(f.sched.ID, in.ID) {
		return res, store.ErrChatScheduleNotFound
	}
	if in.ExpectDefVersion == nil || *in.ExpectDefVersion != f.sched.DefVersion {
		return res, &store.ChatScheduleDefinitionChangedError{Current: f.sched.DefVersion}
	}
	cronExpr, tz := f.sched.CronExpr, f.sched.Timezone
	if in.CronExpr != "" {
		cronExpr = in.CronExpr
	}
	if in.Timezone != "" {
		tz = in.Timezone
	}
	spec, zone, runs, err := store.PreviewChatScheduleCron(cronExpr, tz, time.Now(), store.AdminCronPreviewRuns)
	if err != nil {
		return res, err
	}
	res.NextRuns = runs
	res.After.CronExpr, res.After.Timezone = spec, zone
	res.Changed = spec != f.sched.CronExpr || zone != f.sched.Timezone
	if !res.Changed || in.DryRun {
		if !res.Changed {
			res.After = f.sched
		}
		return res, nil
	}
	f.schedEdit = append(f.schedEdit, in)
	res.After.DefVersion++
	res.After.NextRunAt = runs[0]
	res.Notice = "An administrator changed the schedule. Reason: " + in.Reason
	f.sched = res.After
	return res, nil
}

func (f *fakePatrolStore) AdminListChatSchedules(context.Context, store.AdminChatScheduleFilter) ([]store.AdminChatSchedule, error) {
	return []store.AdminChatSchedule{f.sched}, nil
}

func (f *fakePatrolStore) AdminGetChatSchedule(_ context.Context, id string) (store.AdminChatSchedule, error) {
	if !strings.HasPrefix(f.sched.ID, id) {
		return store.AdminChatSchedule{}, store.ErrChatScheduleNotFound
	}
	return f.sched, nil
}

func (f *fakePatrolStore) AdminSetChatSchedulePrompt(_ context.Context, in store.AdminChatSchedulePromptEdit) (store.AdminChatSchedule, store.AdminChatSchedule, error) {
	f.edits = append(f.edits, in)
	before := f.sched
	f.sched.Prompt = in.Prompt
	f.sched.PromptSHA256 = store.PromptSHA256(in.Prompt)
	f.sched.DefVersion++
	return before, f.sched, nil
}

func (f *fakePatrolStore) AdminSetChatScheduleStatus(_ context.Context, _ store.AdminActor, _ string, status, _ string) (store.AdminChatSchedule, error) {
	f.sched.Status = status
	return f.sched, nil
}

func withFakePatrolStore(t *testing.T) *fakePatrolStore {
	t.Helper()
	prompt := oldPatrolPrompt
	f := &fakePatrolStore{sched: store.AdminChatSchedule{
		ChatSchedule: store.ChatSchedule{
			ID: "5ee926c2-1f76-4221-b7a7-45a23b547742", ConversationID: "dfe4adc7-conv", UserEmail: "owner@client.example",
			Title: "Morning sweep", Prompt: prompt, CronExpr: "30 6 * * 1-5", Timezone: "America/New_York", Status: "active",
		},
		ConversationTitle: "Opti Workbench", DefVersion: 4, PromptSHA256: store.PromptSHA256(prompt),
	}}
	prev := openPatrolStore
	openPatrolStore = func(string) (patrolAdminStore, func(), int) { return f, func() {}, 0 }
	t.Cleanup(func() { openPatrolStore = prev })
	return f
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const oldPatrolPrompt = "13. LOGGING\n- Append to Optimizations/OptLog.\n14. AFTER ANY APPROVAL\n- Offer a memory via propose_memory.\n15. END"

func TestPatrolSetPrompt_AppliesWithBackupAndNotice(t *testing.T) {
	f := withFakePatrolStore(t)
	newPrompt := strings.Replace(oldPatrolPrompt, "- Offer a memory via propose_memory.", "- Append to shared/optimizations/OptLog with shared_file_write.", 1)
	backupDir := filepath.Join(t.TempDir(), "backups")
	var out, errW bytes.Buffer
	code := patrolSetPrompt([]string{"5ee926c2", "--file", writeTemp(t, newPrompt+"\n"), "--expect-sha256", f.sched.PromptSHA256,
		"--reason", "team log moved\nto the shared library", "--backup-dir", backupDir, "--yes"}, strings.NewReader(""), &out, &errW)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out.String(), errW.String())
	}
	if len(f.edits) != 1 {
		t.Fatalf("edits = %d", len(f.edits))
	}
	e := f.edits[0]
	if e.Prompt != newPrompt || e.ID != "5ee926c2-1f76-4221-b7a7-45a23b547742" {
		t.Fatalf("edit = %+v", e)
	}
	if !strings.Contains(e.Notice, `"Morning sweep"`) || !strings.Contains(e.Notice, "team log moved to the shared library") {
		t.Fatalf("notice = %q", e.Notice)
	}
	s := out.String()
	for _, want := range []string{"-- Offer a memory via propose_memory.", "+- Append to shared/optimizations/OptLog", " 14. AFTER ANY APPROVAL", "def_version 4 -> 5"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
	files, _ := os.ReadDir(backupDir)
	if len(files) != 1 {
		t.Fatalf("backups = %v", files)
	}
	b, _ := os.ReadFile(filepath.Join(backupDir, files[0].Name()))
	if string(b) != oldPatrolPrompt {
		t.Fatalf("backup = %q", b)
	}
	if info, _ := files[0].Info(); info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v", info.Mode())
	}
}

func TestPatrolSetPrompt_Refusals(t *testing.T) {
	f := withFakePatrolStore(t)
	good := writeTemp(t, oldPatrolPrompt+"\n16. NEW")
	sha := f.sched.PromptSHA256
	run := func(args ...string) (int, string) {
		var out, errW bytes.Buffer
		code := patrolSetPrompt(args, strings.NewReader("y\n"), &out, &errW)
		return code, out.String() + errW.String()
	}
	cases := []struct {
		name string
		args []string
	}{
		{"no file", []string{"5ee926c2", "--expect-sha256", sha, "--reason", "r", "--yes"}},
		{"no sha", []string{"5ee926c2", "--file", good, "--reason", "r", "--yes"}},
		{"no reason", []string{"5ee926c2", "--file", good, "--expect-sha256", sha, "--yes"}},
		{"stale sha", []string{"5ee926c2", "--file", good, "--expect-sha256", store.PromptSHA256("older"), "--reason", "r", "--yes"}},
		{"empty file", []string{"5ee926c2", "--file", writeTemp(t, "  \n"), "--expect-sha256", sha, "--reason", "r", "--yes"}},
		{"too big", []string{"5ee926c2", "--file", writeTemp(t, strings.Repeat("x", store.MaxAdminChatSchedulePromptBytes+1)), "--expect-sha256", sha, "--reason", "r", "--yes"}},
		{"no id", []string{"--file", good, "--expect-sha256", sha, "--reason", "r", "--yes"}},
		{"extra arg", []string{"5ee926c2", "extra", "--file", good, "--expect-sha256", sha, "--reason", "r", "--yes"}},
		// Not a terminal and no --yes: refuse rather than read "y" from a pipe.
		{"no confirmation", []string{"5ee926c2", "--file", good, "--expect-sha256", sha, "--reason", "r", "--backup-dir", t.TempDir()}},
		{"unwritable backup", []string{"5ee926c2", "--file", good, "--expect-sha256", sha, "--reason", "r", "--yes", "--backup-dir", "/proc/nope"}},
	}
	for _, tc := range cases {
		if code, _ := run(tc.args...); code == 0 {
			t.Errorf("%s: code 0, want a refusal", tc.name)
		}
	}
	if len(f.edits) != 0 {
		t.Fatalf("a refused run wrote: %+v", f.edits)
	}
}

func TestPatrolSetPrompt_DryRunAndNoChange(t *testing.T) {
	f := withFakePatrolStore(t)
	var out bytes.Buffer
	if code := patrolSetPrompt([]string{"5ee926c2", "--file", writeTemp(t, oldPatrolPrompt+"\n16. NEW"), "--expect-sha256", f.sched.PromptSHA256, "--reason", "r", "--dry-run", "--json"},
		strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("dry-run code %d", code)
	}
	var res patrolSetPromptResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !res.DryRun || !res.Changed || !strings.Contains(res.Diff, "+16. NEW") || res.BackupPath != "" {
		t.Fatalf("dry-run result = %+v", res)
	}
	out.Reset()
	if code := patrolSetPrompt([]string{"5ee926c2", "--file", writeTemp(t, oldPatrolPrompt), "--expect-sha256", f.sched.PromptSHA256, "--reason", "r", "--yes"},
		strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), "no change") {
		t.Fatalf("no-change code %d: %s", code, out.String())
	}
	if len(f.edits) != 0 {
		t.Fatalf("dry-run or no-op wrote: %+v", f.edits)
	}
}

func TestPatrolShowAndList(t *testing.T) {
	withFakePatrolStore(t)
	var out bytes.Buffer
	if code := patrolShow([]string{"5ee926c2", "--prompt-only"}, &out); code != 0 || out.String() != oldPatrolPrompt {
		t.Fatalf("prompt-only = %d %q", code, out.String())
	}
	out.Reset()
	if code := patrolShow([]string{"5ee926c2"}, &out); code != 0 || !strings.Contains(out.String(), "sha256 "+store.PromptSHA256(oldPatrolPrompt)) {
		t.Fatalf("show = %d %s", code, out.String())
	}
	out.Reset()
	if code := patrolList([]string{"--json"}, &out); code != 0 {
		t.Fatalf("list code %d", code)
	}
	var list []store.AdminChatSchedule
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || list[0].Prompt != "" || list[0].PromptSHA256 == "" {
		t.Fatalf("list json = %v %+v", err, list)
	}
	out.Reset()
	if code := patrolList(nil, &out); code != 0 || !strings.Contains(out.String(), "5ee926c2") || !strings.Contains(out.String(), "Opti Workbench") {
		t.Fatalf("list table = %s", out.String())
	}
}

func TestLineDiff(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj"
	b := "a\nb\nc\nD\ne\nf\ng\nh\ni\nj\nk"
	got := lineDiff(a, b, 1)
	want := "@@\n c\n-d\n+D\n e\n@@\n j\n+k\n"
	if got != want {
		t.Fatalf("diff =\n%s\nwant\n%s", got, want)
	}
	if lineDiff("x", "x", 3) != "(no line changes)\n" {
		t.Fatal("identical inputs should report no changes")
	}
}

func TestPatrolSetSchedule_Applies(t *testing.T) {
	f := withFakePatrolStore(t)
	var out, errW bytes.Buffer
	code := patrolSetSchedule([]string{"5ee926c2", "--cron", "30 6 * * *", "--expect-def-version", "4",
		"--reason", "the team works\nweekends too", "--yes", "--as", "ops@elcano"}, strings.NewReader(""), &out, &errW)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out.String(), errW.String())
	}
	if len(f.schedEdit) != 1 {
		t.Fatalf("edits = %+v", f.schedEdit)
	}
	e := f.schedEdit[0]
	if e.ID != "5ee926c2" || e.CronExpr != "30 6 * * *" || e.Timezone != "" || *e.ExpectDefVersion != 4 || e.Reason != "the team works weekends too" || e.DryRun {
		t.Fatalf("edit = %+v", e)
	}
	s := out.String()
	for _, want := range []string{"schedule: 30 6 * * 1-5 America/New_York -> 30 6 * * * America/New_York", "next runs:", " 06:30 E", "def_version 4 -> 5", "notice written to the chat"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
	if f.sched.CronExpr != "30 6 * * *" || f.sched.Prompt != oldPatrolPrompt || f.sched.Status != "active" {
		t.Fatalf("patrol = %+v", f.sched)
	}
}

func TestPatrolSetSchedule_DryRunJSONAndNoChange(t *testing.T) {
	f := withFakePatrolStore(t)
	var out bytes.Buffer
	if code := patrolSetSchedule([]string{"5ee926c2", "--timezone", "Europe/London", "--expect-def-version", "4", "--reason", "r", "--dry-run", "--json"},
		strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("dry-run code %d: %s", code, out.String())
	}
	var res patrolSetScheduleResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !res.DryRun || !res.Changed || res.NewCron != "30 6 * * 1-5" || res.NewTimezone != "Europe/London" || len(res.NextRuns) != store.AdminCronPreviewRuns || res.NewDefVersion != 0 {
		t.Fatalf("dry-run result = %+v", res)
	}
	for _, r := range res.NextRuns {
		if !strings.Contains(r, " 06:30 ") {
			t.Fatalf("next run %q, want 06:30 London", r)
		}
	}
	out.Reset()
	if code := patrolSetSchedule([]string{"5ee926c2", "--cron", "30 6 * * 1-5", "--expect-def-version", "4", "--reason", "r", "--yes"},
		strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), "no change") {
		t.Fatalf("no-change code %d: %s", code, out.String())
	}
	if len(f.schedEdit) != 0 {
		t.Fatalf("dry-run or no-op wrote: %+v", f.schedEdit)
	}
}

func TestPatrolSetSchedule_Refusals(t *testing.T) {
	f := withFakePatrolStore(t)
	run := func(args ...string) int {
		var out, errW bytes.Buffer
		return patrolSetSchedule(args, strings.NewReader("y\n"), &out, &errW)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"no cron or timezone", []string{"5ee926c2", "--expect-def-version", "4", "--reason", "r", "--yes"}},
		{"no def_version", []string{"5ee926c2", "--cron", "0 7 * * *", "--reason", "r", "--yes"}},
		{"no reason", []string{"5ee926c2", "--cron", "0 7 * * *", "--expect-def-version", "4", "--yes"}},
		{"stale def_version", []string{"5ee926c2", "--cron", "0 7 * * *", "--expect-def-version", "3", "--reason", "r", "--yes"}},
		{"invalid cron", []string{"5ee926c2", "--cron", "0 7 * *", "--expect-def-version", "4", "--reason", "r", "--yes"}},
		{"too frequent", []string{"5ee926c2", "--cron", "* * * * *", "--expect-def-version", "4", "--reason", "r", "--yes"}},
		{"invalid timezone", []string{"5ee926c2", "--timezone", "Nowhere/Land", "--expect-def-version", "4", "--reason", "r", "--yes"}},
		{"no id", []string{"--cron", "0 7 * * *", "--expect-def-version", "4", "--reason", "r", "--yes"}},
		// Not a terminal and no --yes: refuse rather than read "y" from a pipe.
		{"no confirmation", []string{"5ee926c2", "--cron", "0 7 * * *", "--expect-def-version", "4", "--reason", "r"}},
	}
	for _, tc := range cases {
		if code := run(tc.args...); code == 0 {
			t.Errorf("%s: code 0, want a refusal", tc.name)
		}
	}
	if len(f.schedEdit) != 0 {
		t.Fatalf("a refused run wrote: %+v", f.schedEdit)
	}
}
