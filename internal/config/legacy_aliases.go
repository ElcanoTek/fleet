package config

// LEGACY-ENV-ALIAS: remove after migrate-env — this whole file.
//
// The deprecated CHAT_/CUTLASS_ spellings (ADR-0099): the findings `fleet
// doctor` warns about, and the env-file rewrite `fleet config migrate-env`
// performs. Once every deployment has run `fleet config migrate-env --write`,
// the follow-up that removes alias support deletes this file (see
// docs/CONFIG-REGISTRY.md "Removing the aliases").

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// AliasFinding is one deprecated spelling found set.
type AliasFinding struct {
	Alias     string // the deprecated spelling
	Canonical string // the name to use instead
	Where     string // SourceProcessEnv or SourceEnvFile
	// ReadBy is the alias's only reader when it is reader-specific (the
	// server ignores it).
	ReadBy string
	// ViaSystemd: in the env file under a spelling fleet's own loader does
	// not admit, but the systemd unit exports the file, so it is effective.
	ViaSystemd bool
	// Ignored: in the env file under a spelling fleet's own loader does not
	// admit, and nothing exports the file, so nothing reads it today.
	Ignored bool
}

// String is the one-line warning `fleet doctor` prints. It never contains a
// value.
func (f AliasFinding) String() string {
	s := fmt.Sprintf("%s is a deprecated alias (set in the %s); use %s", f.Alias, f.Where, f.Canonical)
	switch {
	case f.ReadBy != "":
		s += " — only " + f.ReadBy + " reads this spelling; the server ignores it"
	case f.ViaSystemd:
		s += " — it reaches the server only through the systemd unit's EnvironmentFile= (fleet's own loader does not admit this spelling)"
	case f.Ignored:
		s += " — fleet's own loader does not admit this spelling and the env file is not exported by a systemd unit, so the value is ignored today"
	}
	return s
}

// DeprecatedAliasFindings lists every deprecated alias spelling set
// (non-empty) in the shell's process environment, the systemd unit's own
// environment (unit, nil when not on systemd) or envFile, sorted. When the
// unit loads envFile with EnvironmentFile=, every line of it is effective. A
// missing env file is not an error; an unreadable one is.
func DeprecatedAliasFindings(envFile string, unit *UnitEnv) ([]AliasFinding, error) {
	exported := unit != nil && EnvFileIn(envFile, unit.Files)
	file := map[string]string{}
	if envFile != "" {
		vals, err := creds.ReadEnvValues(envFile)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		file = vals
	}
	var out []AliasFinding
	for _, e := range envreg.All() {
		for _, a := range e.DeprecatedAliases() {
			if v, ok := os.LookupEnv(a.Name); ok && strings.TrimSpace(v) != "" {
				out = append(out, AliasFinding{Alias: a.Name, Canonical: e.Name, Where: SourceProcessEnv, ReadBy: a.ReadBy})
			}
			// The unit's own Environment= / other EnvironmentFile= lines (a
			// line from envFile itself is reported as the env-file finding).
			if unit != nil && strings.TrimSpace(unit.Env[a.Name]) != "" &&
				filepath.Clean(strings.TrimPrefix(unit.Origin[a.Name], "EnvironmentFile=")) != filepath.Clean(envFile) {
				out = append(out, AliasFinding{Alias: a.Name, Canonical: e.Name, Where: SourceUnit + " (" + unit.Origin[a.Name] + ")", ReadBy: a.ReadBy})
			}
			if v, ok := file[a.Name]; ok && strings.TrimSpace(v) != "" {
				f := AliasFinding{Alias: a.Name, Canonical: e.Name, Where: SourceEnvFile, ReadBy: a.ReadBy}
				if a.ReadBy == "" && !isAllowedEnvVar(a.Name) {
					f.ViaSystemd, f.Ignored = exported, !exported
				}
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alias != out[j].Alias {
			return out[i].Alias < out[j].Alias
		}
		return out[i].Where < out[j].Where
	})
	return out, nil
}

// MigrationAction is what migrate-env does with one env-file line.
type MigrationAction string

// Migration actions.
const (
	// ActionRename rewrites the alias key to the canonical key in place.
	ActionRename MigrationAction = "rename"
	// ActionDrop removes an alias line whose canonical key is also in the
	// file with the SAME value (the alias was redundant).
	ActionDrop MigrationAction = "drop"
	// ActionLeave keeps a line migrate-env must not touch: a READER-SPECIFIC
	// alias (envreg Alias.ReadBy) that only one reader honours and the server
	// ignores, so renaming it to the canonical key would start applying it to
	// the server. Reported so the operator decides.
	ActionLeave MigrationAction = "leave"
)

// MigrationChange is one line migrate-env acts on. It carries key names and
// line numbers only — never a value.
type MigrationChange struct {
	Line      int // 1-based
	Action    MigrationAction
	From      string // the alias key on that line
	Canonical string
	// LoaderIgnored: fleet's own env-file loader does not admit From, so the
	// line has been effective only where the systemd unit exports the file
	// (EnvironmentFile=, the standard install). It is renamed all the same:
	// the canonical key keeps it working when alias support is removed.
	LoaderIgnored bool
	// ReadBy is the only reader of a reader-specific alias (ActionLeave).
	ReadBy string
}

// EnvMigration is a planned rewrite of one env file.
type EnvMigration struct {
	Path      string
	Changes   []MigrationChange
	Conflicts []string // human-readable, names only; non-empty refuses --write
	out       []string // the rewritten file
}

// Pending reports whether the plan changes the file.
func (m *EnvMigration) Pending() bool {
	for _, c := range m.Changes {
		if c.Action != ActionLeave {
			return true
		}
	}
	return false
}

// PlanEnvMigration reads path and plans the rewrite of every deprecated alias
// line to its canonical key. It refuses (Conflicts) when two spellings of one
// setting are present with DIFFERENT values, since then the rewrite would have
// to pick one. Line order, comments, blank lines, `export ` prefixes and the
// value text are preserved byte for byte; only the key token changes.
func PlanEnvMigration(path string) (*EnvMigration, error) {
	lines, err := creds.ReadEnvLines(path)
	if err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	if lines == nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, fmt.Errorf("env file %s: %w", path, statErr)
		}
	}
	m := &EnvMigration{Path: path, out: append([]string(nil), lines...)}

	// Every value per setting, by spelling, from the file (last assignment
	// wins, like the loader).
	type spellingValue struct {
		name, value string
	}
	perSetting := map[string][]spellingValue{}
	values := map[string]string{}
	for _, ln := range lines {
		k, v, ok := creds.ParseEnvLine(ln)
		if !ok {
			continue
		}
		// A deprecated key assigned twice with different values: the loader
		// takes the last, the rewrite keeps the first line — refuse instead.
		if prev, dup := values[k]; dup && prev != v {
			if renamedAlias(k) {
				m.Conflicts = append(m.Conflicts, fmt.Sprintf("%s is assigned more than once with different values — keep one line and run again", k))
			}
		}
		values[k] = v
	}
	for k, v := range values {
		if e, ok := envreg.Find(k); ok {
			perSetting[e.Name] = append(perSetting[e.Name], spellingValue{k, v})
		}
	}
	for canon, svs := range perSetting {
		if len(svs) < 2 {
			continue
		}
		sort.Slice(svs, func(i, j int) bool { return svs[i].name < svs[j].name })
		for _, sv := range svs[1:] {
			if sv.value != svs[0].value && isDeprecatedPair(svs[0].name, sv.name) {
				m.Conflicts = append(m.Conflicts, fmt.Sprintf("%s and %s are both set with different values (setting %s) — keep one and run again", svs[0].name, sv.name, canon))
			}
		}
	}
	sort.Strings(m.Conflicts)

	renamedTo := map[string]bool{} // canonical keys the rewrite introduces
	for i, ln := range lines {
		k, v, ok := creds.ParseEnvLine(ln)
		if !ok {
			continue
		}
		a, e, isAlias := envreg.AliasOf(k)
		if !isAlias || !a.Deprecated {
			continue
		}
		c := MigrationChange{Line: i + 1, From: k, Canonical: e.Name, LoaderIgnored: !a.EnvFile && a.ReadBy == "", ReadBy: a.ReadBy}
		canonVal, canonInFile := values[e.Name]
		switch {
		case a.ReadBy != "":
			c.Action = ActionLeave
		case (canonInFile && canonVal == v) || renamedTo[e.Name]:
			c.Action = ActionDrop
			m.out[i] = "\x00" // removed below
		default:
			c.Action = ActionRename
			m.out[i] = replaceEnvKey(ln, k, e.Name)
			renamedTo[e.Name] = true
		}
		m.Changes = append(m.Changes, c)
	}
	kept := m.out[:0]
	for _, ln := range m.out {
		if ln != "\x00" {
			kept = append(kept, ln)
		}
	}
	m.out = kept
	return m, nil
}

// isDeprecatedPair reports whether two spellings of one setting are a pair
// migrate-env must reconcile: at least one is a deprecated alias it would
// rename, and neither is a reader-specific alias (left in place, so it cannot
// collide). A conflict between two non-deprecated spellings (FLEET_PERSONA
// and PERSONA) is not migrate-env's business.
func isDeprecatedPair(a, b string) bool {
	deprecated := false
	for _, n := range []string{a, b} {
		al, _, ok := envreg.AliasOf(n)
		if !ok {
			continue
		}
		if al.ReadBy != "" {
			return false
		}
		deprecated = deprecated || al.Deprecated
	}
	return deprecated
}

// renamedAlias reports whether k is a deprecated alias migrate-env renames.
func renamedAlias(k string) bool {
	a, _, ok := envreg.AliasOf(k)
	return ok && a.Deprecated && a.ReadBy == ""
}

// replaceEnvKey swaps the key token of one KEY=VALUE line, keeping leading
// whitespace, an `export ` prefix and everything from the `=` on verbatim.
func replaceEnvKey(line, from, to string) string {
	i := strings.Index(line, from)
	if i < 0 {
		return line
	}
	return line[:i] + to + line[i+len(from):]
}

// ErrMigrationConflicts refuses a write while two spellings disagree.
var ErrMigrationConflicts = errors.New("refusing to rewrite: two spellings of the same setting have different values")

// Write applies the plan: it first copies the file to a timestamped backup
// next to it (same 0600 mode), then writes the rewritten lines atomically
// with the file's owner preserved. It refuses while Conflicts is non-empty.
// It returns the backup path ("" when there was nothing to change).
func (m *EnvMigration) Write(now time.Time) (string, error) {
	if len(m.Conflicts) > 0 {
		return "", ErrMigrationConflicts
	}
	if !m.Pending() {
		return "", nil
	}
	orig, err := os.ReadFile(m.Path) // #nosec G304 — the operator's env file
	if err != nil {
		return "", fmt.Errorf("read %s: %w", m.Path, err)
	}
	backup := m.Path + ".bak-" + now.UTC().Format("20060102T150405Z")
	// O_EXCL: never overwrite an earlier backup (two runs in one second).
	//nolint:gosec // G304/G703: the path is the operator's own env file (--env-file / FLEET_ENV_FILE) plus a fixed suffix, not request input.
	bf, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create backup %s: %w", backup, err)
	}
	if _, err := bf.Write(orig); err != nil {
		_ = bf.Close()
		return "", fmt.Errorf("write backup %s: %w", backup, err)
	}
	if err := bf.Sync(); err != nil {
		_ = bf.Close()
		return "", fmt.Errorf("sync backup %s: %w", backup, err)
	}
	if err := bf.Close(); err != nil {
		return "", fmt.Errorf("close backup %s: %w", backup, err)
	}
	if fi, err := os.Stat(m.Path); err == nil {
		if err := creds.PreserveOwner(backup, fi); err != nil {
			return "", fmt.Errorf("backup %s: %w", backup, err)
		}
	}
	if err := creds.WriteEnvLines(m.Path, m.out); err != nil {
		return backup, fmt.Errorf("write %s: %w (the original is in %s)", m.Path, err, backup)
	}
	return backup, nil
}
