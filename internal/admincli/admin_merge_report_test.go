package admincli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// `fleet admin merge-report` prints what the identity merge recorded, so
// `fleet doctor` can advise on demoted split admins, unknown roles and
// duplicate usernames (ADR-0105; Codex 4504695161 on #2083).
func TestAdminMergeReport(t *testing.T) {
	t.Parallel()

	// A database the merge has not run on (nothing migrated): ran=false.
	var out bytes.Buffer
	if code := adminMergeReport([]string{"--json", "--chat-database-url", config.WithSchema(testdb.Empty(t), config.ChatSchema)}, &out); code != 0 {
		t.Fatalf("merge-report on an empty database: exit %d", code)
	}
	var got struct {
		Ran    bool                  `json:"ran"`
		Report *identity.MergeReport `json:"report"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Ran || got.Report != nil {
		t.Fatalf("empty database: %s (%v)", out.String(), err)
	}

	// A migrated fleet whose merge recorded a demoted admin, an unknown role
	// and a duplicate.
	d := fleetdb.New(t)
	sched := d.SchedDB(t)
	rep := identity.MergeReport{
		Merged:             2,
		DemotedSplitAdmins: []identity.MergeEntry{{Email: "split@x.com", SchedRole: "admin", ChatRole: "member", OpsScope: "run"}},
		UnknownRoles:       []identity.MergeEntry{{Email: "odd@x.com", SchedRole: "boss"}},
		DuplicateUsernames: []identity.MergeEntry{{Email: "Dup@x.com"}},
	}
	raw, _ := json.Marshal(rep)
	if _, err := sched.Conn().ExecContext(t.Context(), `INSERT INTO core.identity_merge (step, done_at, report) VALUES ($1, now(), $2)
		ON CONFLICT (step) DO UPDATE SET report = EXCLUDED.report`, identity.MergeStep, raw); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	out.Reset()
	if code := adminMergeReport([]string{"--json", "--chat-database-url", d.ChatDSN()}, &out); code != 0 {
		t.Fatalf("merge-report --json: exit %d", code)
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !got.Ran || got.Report == nil ||
		len(got.Report.DemotedSplitAdmins) != 1 || got.Report.DemotedSplitAdmins[0].Email != "split@x.com" ||
		len(got.Report.UnknownRoles) != 1 || len(got.Report.DuplicateUsernames) != 1 {
		t.Fatalf("merge-report --json = %s (%v)", out.String(), err)
	}
	out.Reset()
	if code := adminMergeReport([]string{"--chat-database-url", d.ChatDSN()}, &out); code != 0 {
		t.Fatalf("merge-report: exit %d", code)
	}
	for _, want := range []string{"NOT an admin any more: split@x.com", `unknown Operations Center role "boss"`, "Dup@x.com"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("merge-report text lacks %q:\n%s", want, out.String())
		}
	}
}
