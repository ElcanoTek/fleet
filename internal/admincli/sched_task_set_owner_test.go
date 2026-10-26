package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeOwnerStore resolves users from a map and records SetTaskOwner calls.
type fakeOwnerStore struct {
	users    map[string]*models.User
	tasks    map[uuid.UUID][]uuid.UUID // task → the ids a reassignment rewrites
	setErr   error
	gotTask  uuid.UUID
	gotOwner uuid.UUID
}

func (f *fakeOwnerStore) GetUserByUsernameWithContext(_ context.Context, username string) (*models.User, error) {
	if u, ok := f.users[username]; ok {
		return u, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeOwnerStore) SetTaskOwner(_ context.Context, taskID, ownerID uuid.UUID) ([]uuid.UUID, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	ids, ok := f.tasks[taskID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	f.gotTask, f.gotOwner = taskID, ownerID
	return ids, nil
}

func TestSetTaskOwnerCLI(t *testing.T) {
	owner := &models.User{ID: uuid.New(), Username: "owner@example.com"}
	taskID, nextID := uuid.New(), uuid.New()
	newStore := func() *fakeOwnerStore {
		return &fakeOwnerStore{
			users: map[string]*models.User{owner.Username: owner},
			tasks: map[uuid.UUID][]uuid.UUID{taskID: {taskID, nextID}},
		}
	}

	t.Run("reassigns, matching the username case-insensitively", func(t *testing.T) {
		st := newStore()
		var out bytes.Buffer
		if code := setTaskOwner(context.Background(), st, &out, taskID, "Owner@Example.com"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if st.gotTask != taskID || st.gotOwner != owner.ID {
			t.Fatalf("SetTaskOwner(%s, %s), want (%s, %s)", st.gotTask, st.gotOwner, taskID, owner.ID)
		}
		if !strings.Contains(out.String(), "now owned by owner@example.com (2 row(s)") {
			t.Fatalf("output = %q", out.String())
		}
	})
	t.Run("unknown user exits 2", func(t *testing.T) {
		if code := setTaskOwner(context.Background(), newStore(), &bytes.Buffer{}, taskID, "nobody@example.com"); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
	t.Run("unknown task exits 2", func(t *testing.T) {
		if code := setTaskOwner(context.Background(), newStore(), &bytes.Buffer{}, uuid.New(), owner.Username); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
	t.Run("storage failure exits 5", func(t *testing.T) {
		st := newStore()
		st.setErr = errors.New("connection reset")
		if code := setTaskOwner(context.Background(), st, &bytes.Buffer{}, taskID, owner.Username); code != 5 {
			t.Fatalf("exit = %d, want 5", code)
		}
	})
	t.Run("usage errors exit 1", func(t *testing.T) {
		for _, argv := range [][]string{{}, {taskID.String()}, {"not-a-uuid", owner.Username}, {taskID.String(), " "}} {
			if code := schedTaskSetOwner(argv); code != 1 {
				t.Fatalf("schedTaskSetOwner(%q) = %d, want 1", argv, code)
			}
		}
	})
}
