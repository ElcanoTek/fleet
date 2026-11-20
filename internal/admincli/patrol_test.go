package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

type fakePatrolStore struct {
	sched store.AdminChatSchedule
	edits []store.AdminChatSchedulePromptEdit
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
