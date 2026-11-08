package migrations

import (
	"context"
	"database/sql"
	"sort"
)

// Info is one migration in a Report. AppliedAt is the Unix second the ledger
// records for an applied migration, absent for a pending one.
type Info struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	AppliedAt *int64 `json:"applied_at,omitempty"`
}

// Report is the read-only applied-vs-pending view of the one migration
// sequence, served at GET /admin/migrations (both servers) and printed by
// `fleet migrate status`. The first three fields keep the shape the two
// pre-M6 reports had; Layout says which layout produced it.
type Report struct {
	DB             string `json:"db"`              // always "fleet": the one database
	Runner         string `json:"runner"`          // always "fleet"
	MigrationTable string `json:"migration_table"` // always "core.schema_versions"
	Layout         string `json:"layout"`          // always "unified"
	// Adopted reports whether core.schema_versions exists. Before the first
	// start of an M6 release it does not, and the report shows what adoption
	// will record from the two legacy ledgers.
	Adopted bool `json:"adopted"`
	// CurrentVersion is the highest applied version (nil: none).
	CurrentVersion *int   `json:"current_version"`
	Dirty          bool   `json:"dirty"`
	Applied        []Info `json:"applied"`
	Pending        []Info `json:"pending"`
	// Legacy describes the legacy ledgers when not adopted yet.
	Legacy string `json:"legacy,omitempty"`
}

// Status reports applied vs pending migrations. It is strictly READ-ONLY: it
// creates nothing and applies nothing, so it is safe against any database —
// one never migrated (everything pending), one not adopted yet (the legacy
// ledgers read as adoption would record them), one ahead of this build (its
// unknown versions are listed under Applied, so a downgrade is visible).
func Status(ctx context.Context, db *sql.DB) (Report, error) {
	rep := Report{DB: "fleet", Runner: "fleet", MigrationTable: Table, Layout: "unified"}
	r, err := embeddedRunner()
	if err != nil {
		return rep, err
	}
	return r.status(ctx, db)
}

func (r *runner) status(ctx context.Context, db querier) (Report, error) {
	rep := Report{DB: "fleet", Runner: "fleet", MigrationTable: Table, Layout: "unified"}
	exists, err := ledgerExists(ctx, db)
	if err != nil {
		return rep, err
	}
	applied := map[int]Info{}
	if exists {
		rep.Adopted = true
		ledger, err := readLedger(ctx, db)
		if err != nil {
			return rep, err
		}
		for v, row := range ledger {
			at := row.AppliedAt.Unix()
			applied[v] = Info{Version: v, Name: row.Name, AppliedAt: &at}
			rep.Dirty = rep.Dirty || row.Dirty
		}
	} else {
		st, err := readLegacy(ctx, db)
		if err != nil {
			return rep, err
		}
		rep.Legacy = describeLegacy(st)
		rep.Dirty = st.SchedDirty
		// What adoption would record; on a ledger it would refuse, the raw
		// legacy versions, so the report still shows what is there.
		if rows, err := r.plan(st); err == nil {
			for _, row := range rows {
				var at *int64
				if row.AppliedAt != nil {
					u := row.AppliedAt.Unix()
					at = &u
				}
				applied[row.Version] = Info{Version: row.Version, Name: row.Name, AppliedAt: at}
			}
		} else {
			for v, row := range st.Chat {
				at := row.AppliedAt
				applied[v] = Info{Version: v, Name: StreamChat + "/" + row.Name, AppliedAt: &at}
			}
			for _, m := range r.set.list {
				if m.Stream == StreamSched && m.LegacyVersion() <= st.SchedVersion {
					applied[m.Version] = Info{Version: m.Version, Name: m.Name}
				}
			}
			if st.SchedVersion >= 0 {
				if _, ok := r.set.Get(SchedOffset + st.SchedVersion); !ok && st.SchedVersion != 0 {
					v := SchedOffset + st.SchedVersion
					applied[v] = Info{Version: v, Name: "sched/unknown"}
				}
			}
		}
	}
	for _, m := range r.set.list {
		if _, ok := applied[m.Version]; !ok {
			rep.Pending = append(rep.Pending, Info{Version: m.Version, Name: m.Name})
		}
	}
	for _, info := range applied {
		rep.Applied = append(rep.Applied, info)
	}
	sort.Slice(rep.Applied, func(i, j int) bool { return rep.Applied[i].Version < rep.Applied[j].Version })
	if n := len(rep.Applied); n > 0 {
		v := rep.Applied[n-1].Version
		rep.CurrentVersion = &v
	}
	return rep, nil
}

// ChatView is the pre-M6 chat report's shape (DB "chat", runner
// "hand-rolled"), derived from the one sequence: the legacy chat versions,
// under their old numbers and names. `fleet migrate status --json` keeps it
// under "chat" for one release (docs/MIGRATIONS.md).
type ChatView struct {
	DB             string `json:"db"`
	Runner         string `json:"runner"`
	MigrationTable string `json:"migration_table"`
	Applied        []Info `json:"applied"`
	Pending        []Info `json:"pending"`
}

// SchedView is the pre-M6 sched report's shape (DB "sched", runner
// "golang-migrate"), derived the same way: legacy sched versions 1–77.
type SchedView struct {
	DB             string `json:"db"`
	Runner         string `json:"runner"`
	MigrationTable string `json:"migration_table"`
	CurrentVersion *int   `json:"current_version"`
	Dirty          bool   `json:"dirty"`
	Applied        []Info `json:"applied"`
	Pending        []Info `json:"pending"`
}

// streamOf is the legacy stream a version belongs to ("" for the new layout).
func streamOf(v int) string {
	switch {
	case v >= 1 && v < SchedOffset:
		return StreamChat
	case v > SchedOffset && v < FirstNew:
		return StreamSched
	}
	return StreamNew
}

func legacyInfo(i Info) Info {
	m := Migration{Version: i.Version, Name: i.Name, Stream: streamOf(i.Version)}
	return Info{Version: m.LegacyVersion(), Name: m.LegacyName(), AppliedAt: i.AppliedAt}
}

// Chat derives the chat view.
func (rep Report) Chat() ChatView {
	v := ChatView{DB: "chat", Runner: "hand-rolled", MigrationTable: "schema_migrations"}
	for _, i := range rep.Applied {
		if streamOf(i.Version) == StreamChat {
			v.Applied = append(v.Applied, legacyInfo(i))
		}
	}
	for _, i := range rep.Pending {
		if streamOf(i.Version) == StreamChat {
			v.Pending = append(v.Pending, legacyInfo(i))
		}
	}
	return v
}

// Sched derives the sched view. Its CurrentVersion is the highest applied
// legacy sched version, as golang-migrate's single row recorded it.
func (rep Report) Sched() SchedView {
	v := SchedView{DB: "sched", Runner: "golang-migrate", MigrationTable: "schema_migrations", Dirty: rep.Dirty}
	for _, i := range rep.Applied {
		if streamOf(i.Version) == StreamSched {
			li := legacyInfo(i)
			li.AppliedAt = nil
			v.Applied = append(v.Applied, li)
			cur := li.Version
			v.CurrentVersion = &cur
		}
	}
	for _, i := range rep.Pending {
		if streamOf(i.Version) == StreamSched {
			v.Pending = append(v.Pending, legacyInfo(i))
		}
	}
	return v
}

// Body returns the up SQL of version v of this build (tests that replay one
// legacy migration inside a rolled-back transaction).
func Body(v int) (string, bool) {
	set, err := Embedded()
	if err != nil {
		return "", false
	}
	m, ok := set.Get(v)
	return m.Up, ok
}
