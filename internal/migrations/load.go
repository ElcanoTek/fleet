// Package migrations is fleet's one schema-migration runner and its one
// migration directory (#2049 M6, ADR-0107, docs/MIGRATIONS.md).
//
// Every migration of the one database is a version in one sequence, recorded
// in one ledger, core.schema_versions:
//
//   - sql/legacy/chat/NNN_name.sql — the chat store's forward-only migrations
//     of the old hand-rolled runner, versions 1–99 (version N);
//   - sql/legacy/sched/NNN_name.{up,down}.sql — the scheduler's migrations of
//     the old golang-migrate-format runner, versions 101–177 (version 100+N);
//   - sql/NNNN_name.up.sql + .down.sql — every migration since, version 200
//     and up, each with both halves.
//
// Legacy files are frozen: their bytes are what every installed database
// ran, and they run exactly as before, each with its store's schema as the
// transaction's search_path (SET LOCAL). The merged order (all of chat, then
// all of sched) is the order a fresh install has always run them in, which is
// load-bearing: sched 077 reads chat.chat_schedules, and the identity merge
// needs core. New files run with search_path pg_catalog and name every object
// with its schema (chat.x, sched.y, core.z).
//
// The package is a leaf: it imports neither store, so both (and the tests'
// template builders) call it.
package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/dbschema"
)

//go:embed sql
var embedded embed.FS

// Version numbering of the one sequence.
const (
	// SchedOffset maps legacy sched migration N to version SchedOffset+N.
	SchedOffset = 100
	// LegacyLatest is the last legacy version (sched 077). Nothing below it
	// has a down migration the runner will run: `fleet migrate down` stops
	// here.
	LegacyLatest = SchedOffset + 77
	// FirstNew is the first version of a migration in the new layout.
	FirstNew = 200

	// ChatSentinel and SchedSentinel are the rows the runner writes into the
	// two legacy ledgers (chat.schema_migrations, sched.schema_migrations), in
	// the transaction of the first migration at or above FirstNew, so a
	// pre-M6 binary refuses to boot on the database: its chat runner refuses a
	// version newer than it knows (100 > 99), and its sched runner finds no
	// migration 178.
	ChatSentinel     = 100
	ChatSentinelName = "moved_to_core_schema_versions"
	SchedSentinel    = 178
)

// Stream names which store a legacy migration belongs to.
const (
	StreamChat  = "chat"
	StreamSched = "sched"
	StreamNew   = "" // the new layout: no search_path of its own
)

// Migration is one version of the sequence.
type Migration struct {
	Version  int
	Name     string // "chat/001_initial", "sched/001_initial_schema", "0200_add_x"
	Stream   string // StreamChat, StreamSched or StreamNew
	Up       string
	Down     string // "" when there is none (legacy chat)
	Checksum string // hex SHA-256 of Up
}

// SearchPath is the search_path the migration's transaction runs with.
func (m Migration) SearchPath() string {
	switch m.Stream {
	case StreamChat, StreamSched:
		return m.Stream
	default:
		return "pg_catalog"
	}
}

// LegacyName is the file stem the old runners knew the migration by
// ("001_initial"), or Name for a new one.
func (m Migration) LegacyName() string {
	if _, rest, ok := strings.Cut(m.Name, "/"); ok {
		return rest
	}
	return m.Name
}

// LegacyVersion is the version the old runner recorded (sched 101 → 1), or
// Version for chat and new migrations.
func (m Migration) LegacyVersion() int {
	if m.Stream == StreamSched {
		return m.Version - SchedOffset
	}
	return m.Version
}

// Set is a loaded, validated migration sequence in ascending version order.
type Set struct {
	list      []Migration
	byVersion map[int]int // version → index
	fsys      fs.FS
}

// All returns the sequence, ascending.
func (s *Set) All() []Migration { return append([]Migration(nil), s.list...) }

// Get returns the migration of version v.
func (s *Set) Get(v int) (Migration, bool) {
	i, ok := s.byVersion[v]
	if !ok {
		return Migration{}, false
	}
	return s.list[i], true
}

// Latest is the highest version of the set.
func (s *Set) Latest() int {
	if len(s.list) == 0 {
		return 0
	}
	return s.list[len(s.list)-1].Version
}

var (
	chatFilePattern  = regexp.MustCompile(`^(\d{3})_[A-Za-z0-9_]+\.sql$`)
	schedFilePattern = regexp.MustCompile(`^([0-9]+)_(.*)\.(down|up)\.sql$`)
	newFilePattern   = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$`)
	// noTxDirective is the (not yet supported) opt-out of the per-migration
	// transaction; see ADR-0107's future work.
	noTxDirective = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*fleet:no-transaction\b`)
)

var (
	defaultOnce sync.Once
	defaultSet  *Set
	errEmbedded error
)

// Embedded is the migration set this build ships.
func Embedded() (*Set, error) {
	defaultOnce.Do(func() { defaultSet, errEmbedded = Load(embedded) })
	return defaultSet, errEmbedded
}

// Fingerprint identifies this build's migration set (a hash of every embedded
// file). internal/testdb keys its migrated template databases by it.
func Fingerprint() string { return dbschema.Fingerprint(embedded) }

// Load reads and validates a migration tree laid out like the embedded one
// (sql/legacy/chat, sql/legacy/sched, sql). It refuses a build it could not
// run safely: a duplicate version, a legacy number out of its range, a new
// migration below FirstNew or missing its .up or .down half, a file in sql/
// that is not a migration, or a -- fleet:no-transaction directive.
func Load(fsys fs.FS) (*Set, error) {
	var out []Migration
	chat, err := loadChat(fsys, "sql/legacy/chat")
	if err != nil {
		return nil, err
	}
	out = append(out, chat...)
	sched, err := loadSched(fsys, "sql/legacy/sched")
	if err != nil {
		return nil, err
	}
	out = append(out, sched...)
	fresh, err := loadNew(fsys, "sql")
	if err != nil {
		return nil, err
	}
	out = append(out, fresh...)

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	set := &Set{list: out, byVersion: map[int]int{}, fsys: fsys}
	for i, m := range out {
		if _, dup := set.byVersion[m.Version]; dup {
			return nil, fmt.Errorf("migrations: duplicate version %d (%s)", m.Version, m.Name)
		}
		if noTxDirective.MatchString(m.Up) || noTxDirective.MatchString(m.Down) {
			return nil, fmt.Errorf("migrations: %s carries -- fleet:no-transaction, which this runner does not support yet (ADR-0107): every migration runs in one transaction with its ledger row", m.Name)
		}
		set.byVersion[m.Version] = i
	}
	return set, nil
}

func checksum(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

func readDir(fsys fs.FS, dir string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: read %s: %w", dir, err)
	}
	return entries, nil
}

// loadChat reads the legacy chat files: NNN_name.sql, forward only.
func loadChat(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := readDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := chatFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("migrations: %s/%s is not a legacy chat migration name (NNN_name.sql)", dir, e.Name())
			}
			continue
		}
		n, _ := strconv.Atoi(m[1]) // three digits
		if n < 1 || n >= ChatSentinel {
			return nil, fmt.Errorf("migrations: legacy chat migration %s is outside 1–99", e.Name())
		}
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		out = append(out, Migration{
			Version:  n,
			Name:     StreamChat + "/" + strings.TrimSuffix(e.Name(), ".sql"),
			Stream:   StreamChat,
			Up:       string(body),
			Checksum: checksum(string(body)),
		})
	}
	return out, nil
}

// loadSched reads the legacy sched files (golang-migrate's naming).
func loadSched(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := readDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	byN := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := schedFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("migrations: %s/%s is not a legacy sched migration name (NNN_name.up.sql / .down.sql)", dir, e.Name())
			}
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || SchedOffset+n >= FirstNew {
			return nil, fmt.Errorf("migrations: legacy sched migration %s is outside 1–%d", e.Name(), FirstNew-SchedOffset-1)
		}
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		mig := byN[n]
		if mig == nil {
			mig = &Migration{Version: SchedOffset + n, Name: StreamSched + "/" + m[1] + "_" + m[2], Stream: StreamSched}
			byN[n] = mig
		}
		switch m[3] {
		case "up":
			if mig.Up != "" {
				return nil, fmt.Errorf("migrations: duplicate legacy sched migration %s", e.Name())
			}
			mig.Up = string(body)
			mig.Name = StreamSched + "/" + m[1] + "_" + m[2]
			mig.Checksum = checksum(mig.Up)
		case "down":
			if mig.Down != "" {
				return nil, fmt.Errorf("migrations: duplicate legacy sched migration %s", e.Name())
			}
			mig.Down = string(body)
		}
	}
	out := make([]Migration, 0, len(byN))
	for _, m := range byN {
		if m.Up == "" {
			return nil, fmt.Errorf("migrations: legacy sched migration %d has no .up.sql", m.Version-SchedOffset)
		}
		out = append(out, *m)
	}
	return out, nil
}

// loadNew reads the new layout: NNNN_name.up.sql and .down.sql, version >=
// FirstNew, both halves present. Anything else in sql/ but the legacy
// directory and non-.sql files (a README) is refused.
func loadNew(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := readDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	byV := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			if e.Name() != "legacy" {
				return nil, fmt.Errorf("migrations: unexpected directory %s/%s (new migrations live directly in %s)", dir, e.Name(), dir)
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := newFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations: %s/%s is not a migration name (NNNN_name.up.sql / NNNN_name.down.sql, lower case)", dir, e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		if v < FirstNew {
			return nil, fmt.Errorf("migrations: %s: new migrations start at version %d", e.Name(), FirstNew)
		}
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		name := m[1] + "_" + m[2]
		mig := byV[v]
		if mig == nil {
			mig = &Migration{Version: v, Name: name, Stream: StreamNew}
			byV[v] = mig
		}
		if mig.Name != name {
			return nil, fmt.Errorf("migrations: version %d has two names (%s, %s)", v, mig.Name, name)
		}
		switch m[3] {
		case "up":
			mig.Up = string(body)
			mig.Checksum = checksum(mig.Up)
		case "down":
			mig.Down = string(body)
		}
	}
	out := make([]Migration, 0, len(byV))
	for v, m := range byV {
		if strings.TrimSpace(m.Up) == "" {
			return nil, fmt.Errorf("migrations: version %d has no (or an empty) %s.up.sql", v, m.Name)
		}
		if strings.TrimSpace(m.Down) == "" {
			return nil, fmt.Errorf("migrations: version %d has no (or an empty) %s.down.sql: every migration from %d on ships its down half", v, m.Name, FirstNew)
		}
		out = append(out, *m)
	}
	return out, nil
}
