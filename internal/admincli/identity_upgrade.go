package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/store"
)

// openChatStoreDSN is the ONE way an operator verb opens the chat store: it
// migrates it (store.Open) and then completes the upgrade to the one identity
// table (#2049 M4) before the verb touches an account.
//
// Until the identity merge has run (identity.MergeLegacySchedUsers, which the
// scheduler's open runs), the scheduler's legacy accounts are still in
// sched.users. A verb that changed core.users before it — `fleet admin rm`,
// `fleet chat user del` — would be undone by the merge that follows, which
// folds the legacy row back in. Since #2049 M6 store.Open runs the one
// migration runner, which migrates chat, core and sched alike, so the merge
// can run right here, the same as `fleet serve`'s first start would (and it
// is one read once it has run).
func openChatStoreDSN(dsn string) (*store.Store, error) {
	st, err := store.Open(dsn, store.DefaultPoolConfig())
	if err != nil {
		return nil, err
	}
	if err := completeIdentityMerge(context.Background(), dsn); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// completeIdentityMerge migrates the database of chatDSN and runs the
// identity merge on it when the merge is pending: the scheduler's legacy sched.users is there next to
// core.users. A database without either (empty), or already merged, is left
// alone.
func completeIdentityMerge(ctx context.Context, chatDSN string) error {
	pending, err := identityMergePending(ctx, chatDSN)
	if err != nil || !pending {
		return err
	}
	db, err := sql.Open("pgx", chatDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// The merge reads the scheduler's legacy table in its latest legacy shape
	// (sched 072's enabled, …): migrate first, under the migration lock, so it
	// never runs against a partly migrated database (a box from before M6
	// whose scheduler is behind, here for a restore). Only through the legacy
	// versions while below the boundary: a restore of a pre-M6 dump may
	// follow, which a database carried past it would refuse.
	if err := migrations.UpLegacy(ctx, db); err != nil {
		return fmt.Errorf("migrate the database before merging the scheduler's accounts: %w", err)
	}
	res, err := identity.MergeLegacySchedUsers(ctx, db, config.SchedSchema)
	if err != nil {
		return fmt.Errorf("complete the upgrade to the one identity table (merge the scheduler's accounts into core.users): %w — start fleet once to complete it, then retry", err)
	}
	if !res.Ready {
		return fmt.Errorf("the scheduler's accounts (sched.users) are not merged into core.users yet and could not be merged now: start fleet once to complete the upgrade, then retry")
	}
	return nil
}

// identityMergePending reports whether the database still has the
// scheduler's legacy accounts table next to the one identity table.
func identityMergePending(ctx context.Context, dsn string) (bool, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var legacy, core bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('`+config.SchedSchema+`.users') IS NOT NULL, to_regclass('`+config.IdentitySchema+`.users') IS NOT NULL`).Scan(&legacy, &core); err != nil {
		return false, fmt.Errorf("probe the identity tables: %w", err)
	}
	return legacy && core, nil
}
