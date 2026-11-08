package identity

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIdentityTablesAreSchemaQualified fails on any bare reference to an
// identity table in the stores' Go code (#2049 M4). The identity tables live
// in schema core, which is on no connection's search_path, so a bare `users`
// either fails — or, worse, in the scheduler resolves to a table of its own
// schema: before the merge renames it, the legacy sched.users the Operations
// Center must never read again. Every statement names core.<table>.
//
// Migrations are exempt (their history predates the move), and so are tests
// that replay an old migration.
func TestIdentityTablesAreSchemaQualified(t *testing.T) {
	bare := regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE|TABLE|EXISTS)\s+(users|external_access_state|external_auth_epochs|external_logout_events|external_team_renames)\b`)
	roots := []string{"../sched", "../store", "../httpapi", "../admincli", "../../cmd/fleet"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "migrations" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(body), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				if m := bare.FindString(line); m != "" {
					t.Errorf("%s:%d: bare identity table %q — name it core.<table>", path, i+1, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
