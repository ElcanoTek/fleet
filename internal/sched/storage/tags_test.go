package storage

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func mkTask(tags ...string) *models.Task {
	return &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), Tags: tags}
}

// TestTaskTagsRoundTrip pins the JSONB tags column: empty round-trips as empty,
// populated round-trips intact.
func TestTaskTagsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	untagged := mkTask()
	tagged := mkTask("nightly", "prod")
	for _, tk := range []*models.Task{untagged, tagged} {
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	if got, err := store.GetTask(untagged.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if len(got.Tags) != 0 {
		t.Errorf("untagged task should round-trip with no tags, got %v", got.Tags)
	}
	if got, err := store.GetTask(tagged.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if len(got.Tags) != 2 || got.Tags[0] != "nightly" || got.Tags[1] != "prod" {
		t.Errorf("tags did not round-trip: %v", got.Tags)
	}
}

// TestTaskTagsFilterAndSemantics pins ?tag= AND-containment + the catalogue.
func TestTaskTagsFilterAndSemantics(t *testing.T) {
	store, database := newTestStore(t)
	ctx := context.Background()

	both := mkTask("nightly", "prod")
	onlyNightly := mkTask("nightly")
	none := mkTask()
	for _, tk := range []*models.Task{both, onlyNightly, none} {
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	// AND-semantics: requiring both nightly AND prod matches only `both`.
	tasks, _, err := database.GetTasksFiltered(ctx, db.TaskFilter{Tags: []string{"nightly", "prod"}}, 100, 0)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != both.ID {
		t.Errorf("AND filter [nightly,prod] should match exactly the both-tagged task, got %d", len(tasks))
	}

	// Single tag matches both carriers.
	tasks, _, err = database.GetTasksFiltered(ctx, db.TaskFilter{Tags: []string{"nightly"}}, 100, 0)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("filter [nightly] should match 2 tasks, got %d", len(tasks))
	}

	// Catalogue counts.
	cat, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	counts := map[string]int{}
	for _, c := range cat {
		counts[c.Tag] = c.TaskCount
	}
	if counts["nightly"] != 2 || counts["prod"] != 1 {
		t.Errorf("catalogue counts wrong: %v", counts)
	}
	// Busiest-first ordering.
	if len(cat) >= 2 && cat[0].TaskCount < cat[1].TaskCount {
		t.Errorf("catalogue should be sorted by count desc, got %v", cat)
	}
}

// TestUpdateTaskTags pins atomic add/remove with removal-wins and re-normalization.
func TestUpdateTaskTags(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	tk := mkTask("keep", "drop")
	if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Add {new, also} and remove {drop}; "keep" stays.
	upd, err := store.UpdateTaskTags(ctx, tk.ID, []string{"new", "also"}, []string{"drop"})
	if err != nil {
		t.Fatalf("update tags: %v", err)
	}
	got := append([]string(nil), upd.Tags...)
	sort.Strings(got)
	want := []string{"also", "keep", "new"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("after add/remove got %v, want %v", upd.Tags, want)
	}

	// Removal wins when a tag is in both add and remove.
	upd, err = store.UpdateTaskTags(ctx, tk.ID, []string{"x"}, []string{"x"})
	if err != nil {
		t.Fatalf("update tags: %v", err)
	}
	for _, tag := range upd.Tags {
		if tag == "x" {
			t.Errorf("tag in both add+remove should be removed, got %v", upd.Tags)
		}
	}
}

// TestTagCatalogueScope pins creator-scoped tag enumeration (#1082): a scoped
// principal sees only tags from tasks it created, while fleet-wide scope
// returns all tags.
func TestTagCatalogueScope(t *testing.T) {
	store, database := newTestStore(t)
	ctx := context.Background()

	userA := uuid.New()
	userB := uuid.New()
	keyA := "fleet_task_key_a"

	t1 := mkTask("shared", "alpha")
	t1.CreatedBy = &userA
	t2 := mkTask("alpha", "user-a-only")
	t2.CreatedBy = &userA

	t3 := mkTask("shared", "beta")
	t3.CreatedBy = &userB

	t4 := mkTask("shared", "from-key")
	t4.CreatedByKeyID = &keyA

	t5 := mkTask("unowned")

	for _, tk := range []*models.Task{t1, t2, t3, t4, t5} {
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	// Fleet-wide: see all tags across all users, keys, and unowned tasks.
	catAll, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{})
	if err != nil {
		t.Fatalf("fleet-wide catalogue: %v", err)
	}
	countsAll := map[string]int{}
	for _, c := range catAll {
		countsAll[c.Tag] = c.TaskCount
	}
	if countsAll["shared"] != 3 || countsAll["alpha"] != 2 || countsAll["beta"] != 1 ||
		countsAll["user-a-only"] != 1 || countsAll["from-key"] != 1 || countsAll["unowned"] != 1 {
		t.Errorf("fleet-wide counts mismatch: %v", countsAll)
	}

	// User A scope: sees alpha (2), shared (1), user-a-only (1). Nothing from B, keyA, or unowned.
	catA, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{VisibleToUserID: &userA})
	if err != nil {
		t.Fatalf("user A catalogue: %v", err)
	}
	countsA := map[string]int{}
	for _, c := range catA {
		countsA[c.Tag] = c.TaskCount
	}
	if len(countsA) != 3 || countsA["alpha"] != 2 || countsA["shared"] != 1 || countsA["user-a-only"] != 1 {
		t.Errorf("user A counts mismatch: got %v", countsA)
	}

	// User B scope: sees beta (1), shared (1).
	catB, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{VisibleToUserID: &userB})
	if err != nil {
		t.Fatalf("user B catalogue: %v", err)
	}
	countsB := map[string]int{}
	for _, c := range catB {
		countsB[c.Tag] = c.TaskCount
	}
	if len(countsB) != 2 || countsB["beta"] != 1 || countsB["shared"] != 1 {
		t.Errorf("user B counts mismatch: got %v", countsB)
	}

	// Key A scope: sees from-key (1), shared (1).
	catKey, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{VisibleToKeyID: &keyA})
	if err != nil {
		t.Fatalf("key A catalogue: %v", err)
	}
	countsKey := map[string]int{}
	for _, c := range catKey {
		countsKey[c.Tag] = c.TaskCount
	}
	if len(countsKey) != 2 || countsKey["from-key"] != 1 || countsKey["shared"] != 1 {
		t.Errorf("key A counts mismatch: got %v", countsKey)
	}

	// Unknown user: empty catalogue.
	unknownUser := uuid.New()
	catEmpty, err := database.GetTagCatalogue(ctx, db.TagCatalogueScope{VisibleToUserID: &unknownUser})
	if err != nil {
		t.Fatalf("empty catalogue: %v", err)
	}
	if len(catEmpty) != 0 {
		t.Errorf("expected empty catalogue for unknown user, got %v", catEmpty)
	}
}
