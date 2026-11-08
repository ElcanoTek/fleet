package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

// openChatStoreDSN is the ONE way an operator verb opens the chat store: it
// migrates it (store.Open) and then completes the upgrade to the one identity
// table (#2049 M4) before the verb touches an account.
//
// Between chat migration 099 (core.users exists) and the scheduler's first
// open on this release (identity.MergeLegacySchedUsers), the scheduler's
// legacy accounts are still in sched.users. A verb that changed core.users
// in that window — `fleet admin rm`, `fleet chat user del` — would be undone
// by the merge that follows, which folds the legacy row back in. So when the
// database still has an unmerged sched.users, this opens the scheduler store
// here, in this process: its open runs the scheduler's migrations and the
// merge, the same as `fleet serve`'s first start would.
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

// completeIdentityMerge runs the scheduler's identity merge on the database
// of chatDSN when it is still pending: sched.users (the legacy table) is
// there and core.users is too (the chat store migrated). A database with no
// scheduler schema, or one already merged (sched.users renamed away), is
// left alone.
func completeIdentityMerge(ctx context.Context, chatDSN string) error {
	pending, err := identityMergePending(ctx, chatDSN)
	if err != nil || !pending {
		return err
	}
	sd := &scheddb.Database{}
	if err := sd.Init(config.WithSchema(chatDSN, config.SchedSchema), scheddb.DefaultPoolConfig()); err != nil {
		return fmt.Errorf("complete the upgrade to the one identity table (merge the scheduler's accounts into core.users): %w — start fleet once to complete it, then retry", err)
	}
	defer sd.Close()
	if m := sd.IdentityMerge(); !m.Ready {
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
