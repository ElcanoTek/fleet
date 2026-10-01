package db

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskSearchTerms pins how a Recent Tasks search is split: words match
// independently, a quoted run stays one phrase, blanks never become a term,
// and an over-long paste is capped rather than multiplying the scan.
func TestTaskSearchTerms(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"weekly sales", []string{"weekly", "sales"}},
		{"  weekly \t  sales  ", []string{"weekly", "sales"}},
		{`"daily deal" scan`, []string{"daily deal", "scan"}},
		{`report "daily deal`, []string{"report", "daily deal"}},
		{`""`, nil},
		{"a b c d e f g h i j", []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
	}
	for _, c := range cases {
		if got := taskSearchTerms(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("taskSearchTerms(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTaskSearchFindsJobsTheWayPeopleRememberThem is the Recent Tasks search
// against a real database. Users reported jobs they could not find; each case
// here is one way of remembering a job that the old single-substring search
// (title, prompt or id ILIKE '%whole input%') answered with an empty board:
// words in another order, a doubled space, a tag, the description, the
// creator shown in the Created By column. The literal-% case is the opposite
// failure — a wildcard that matched far more than was typed.
func TestTaskSearchFindsJobsTheWayPeopleRememberThem(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	alice := uuid.New()
	if err := db.AddUser(ctx, &models.User{ID: alice, Username: "alice@example.com", Role: "client", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("add user: %v", err)
	}
	add := func(title, prompt, description string, tags []string, createdBy *uuid.UUID) string {
		t.Helper()
		task := &models.Task{
			ID: uuid.New(), Title: title, Prompt: prompt, Description: description, Tags: tags,
			CreatedBy: createdBy, Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), Timezone: "UTC",
		}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("add task %q: %v", title, err)
		}
		return task.ID.String()
	}
	weekly := add("Weekly sales report", "Summarise the week's sales.", "", []string{"finance"}, nil)
	deals := add("", "Daily deal health scan\nCheck every open deal.", "Runs for the ops team", nil, &alice)
	target := add("Q3 100% target", "Track the quarter.", "", nil, nil)
	plain := add("Q3 1000 units", "Count units.", "", nil, nil)

	search := func(q string) []string {
		t.Helper()
		tasks, total, err := db.GetTasksFiltered(ctx, TaskFilter{Query: &q}, 50, 0)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if total != len(tasks) {
			t.Fatalf("search %q: total %d disagrees with %d rows on one page", q, total, len(tasks))
		}
		ids := make([]string, 0, len(tasks))
		for _, task := range tasks {
			ids = append(ids, task.ID.String())
		}
		sort.Strings(ids)
		return ids
	}
	only := func(ids ...string) []string { sort.Strings(ids); return append([]string{}, ids...) }

	for _, c := range []struct {
		name, q string
		want    []string
	}{
		{"exact phrase still works", "weekly sales", only(weekly)},
		{"words in another order", "sales weekly", only(weekly)},
		{"doubled whitespace", "weekly   sales", only(weekly)},
		{"every word must match", "weekly deal", only()},
		{"quoted phrase keeps its order", `"sales weekly"`, only()},
		{"a tag", "finance", only(weekly)},
		{"the description", "ops team", only(deals)},
		{"the prompt's first line (the untitled row's label)", "daily deal", only(deals)},
		{"the creator shown in Created By", "alice", only(deals)},
		{"a short id prefix", weekly[:8], only(weekly)},
		{"a full id", deals, only(deals)},
		{"% is literal, not a wildcard", "100%", only(target)},
		{"_ is literal, not a wildcard", "Q3_1000", only()},
		{"case-insensitive", "WEEKLY", only(weekly)},
		{"plain substring", "units", only(plain)},
	} {
		if got := search(c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: search %q = %v, want %v", c.name, c.q, got, c.want)
		}
	}

	// Search ANDs with the other filters like every filter does: a matching
	// row in another status stays hidden.
	q, status := "weekly", string(models.TaskStatusPending)
	if tasks, _, err := db.GetTasksFiltered(ctx, TaskFilter{Query: &q, Status: &status}, 50, 0); err != nil || len(tasks) != 0 {
		t.Fatalf("search + status: %d rows, err %v — want none (the match is a success row)", len(tasks), err)
	}
}
