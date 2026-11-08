package identity

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The scheduler keyed some rows by a user's sched username as text, not by
// the account UUID. The merge turns that username into the account's email
// (NormalizeEmail: trimmed, lower-cased), so a legacy username that was not
// already in that form ("Brad@X.com", " ops@x.com") would no longer match
// its own rows. rewritePrincipals renames them in the merge's transaction,
// for exactly the usernames the merge changed (an API key's name, "admin" or
// a run label is never touched):
//
//   - prompt_library.owner_username — the owner's private and workspace
//     prompts. Prompts are never dropped: when the account already has a
//     prompt of the same name (case-insensitively, the table's unique key),
//     the moved one is renamed "<name> (from <old username>)" (then " 2",
//     " 3", … if that is taken too), and listed in the report.
//   - budgets.principal_id of scope user — the user's budgets. When the
//     account already has a budget for the same window, the two are merged
//     into the existing row keeping the stricter bound of each kind (a
//     configured bound beats none; of two, the lower), the moved row is
//     deleted, and the merge is listed in the report.
//   - audit labels: agent_notes.created_by / updated_by,
//     agent_note_proposals.proposed_by / decided_by,
//     task_feedback.created_by and task_learned_instructions.activated_by.
//
// Free text that merely mentions a username (a task's "stopped by …" reason)
// is history and stays as written.

// principalColumns are the plain-rename audit columns: table, column.
var principalColumns = [][2]string{
	{"agent_notes", "created_by"},
	{"agent_notes", "updated_by"},
	{"agent_note_proposals", "proposed_by"},
	{"agent_note_proposals", "decided_by"},
	{"task_feedback", "created_by"},
	{"task_learned_instructions", "activated_by"},
}

// PromptRename is a moved prompt that had to change its name.
type PromptRename struct {
	Owner string `json:"owner"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// BudgetMerge is a moved user budget folded into the account's existing one.
type BudgetMerge struct {
	Principal string `json:"principal"`
	Window    string `json:"window"`
	From      string `json:"from"`
}

func rewritePrincipals(ctx context.Context, tx *sql.Tx, schema string, renames map[string]string, rep *MergeReport) error {
	if len(renames) == 0 {
		return nil
	}
	exists := func(table string) (bool, error) {
		var ok bool
		err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&ok)
		return ok, err
	}
	if ok, err := exists("prompt_library"); err != nil {
		return err
	} else if ok {
		if err := movePrompts(ctx, tx, schema, renames, rep); err != nil {
			return fmt.Errorf("identity merge: prompt owners: %w", err)
		}
	}
	if ok, err := exists("budgets"); err != nil {
		return err
	} else if ok {
		if err := moveBudgets(ctx, tx, schema, renames, rep); err != nil {
			return fmt.Errorf("identity merge: user budgets: %w", err)
		}
	}
	for _, tc := range principalColumns {
		ok, err := exists(tc[0])
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for from, to := range renames {
			// Identifiers are the fixed principalColumns and the checked schema.
			res, err := tx.ExecContext(ctx, `UPDATE `+schema+`.`+tc[0]+` SET `+tc[1]+` = $2 WHERE `+tc[1]+` = $1`, from, to) //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
			if err != nil {
				return fmt.Errorf("identity merge: %s.%s: %w", tc[0], tc[1], err)
			}
			n, _ := res.RowsAffected()
			rep.RewrittenPrincipals += int(n)
		}
	}
	return nil
}

func movePrompts(ctx context.Context, tx *sql.Tx, schema string, renames map[string]string, rep *MergeReport) error {
	type prompt struct {
		id          uuid.UUID
		owner, name string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, owner_username, name FROM `+schema+`.prompt_library ORDER BY created_at, id`) //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
	if err != nil {
		return err
	}
	var all []prompt
	for rows.Next() {
		var p prompt
		if err := rows.Scan(&p.id, &p.owner, &p.name); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// taken is the (owner, lower(name)) key of every row that stays put.
	taken := map[string]bool{}
	key := func(owner, name string) string { return owner + "\x00" + strings.ToLower(name) }
	for _, p := range all {
		if _, moves := renames[p.owner]; !moves {
			taken[key(p.owner, p.name)] = true
		}
	}
	for _, p := range all {
		to, moves := renames[p.owner]
		if !moves {
			continue
		}
		name := p.name
		if taken[key(to, name)] {
			name = freePromptName(p.name, p.owner, func(n string) bool { return taken[key(to, n)] })
			rep.RenamedPrompts = append(rep.RenamedPrompts, PromptRename{Owner: to, From: p.name, To: name})
		}
		taken[key(to, name)] = true
		if _, err := tx.ExecContext(ctx, `UPDATE `+schema+`.prompt_library SET owner_username = $2, name = $3 WHERE id = $1`, p.id, to, name); err != nil { //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
			return err
		}
		rep.RewrittenPrincipals++
	}
	return nil
}

// freePromptName is "<name> (from <old owner>)", then with " 2", " 3", …,
// cut to the table's 120-character limit.
func freePromptName(name, from string, taken func(string) bool) string {
	for i := 1; ; i++ {
		suffix := " (from " + from + ")"
		if i > 1 {
			suffix += fmt.Sprintf(" %d", i)
		}
		base := name
		for utf8.RuneCountInString(base)+utf8.RuneCountInString(suffix) > 120 && base != "" {
			_, size := utf8.DecodeLastRuneInString(base)
			base = base[:len(base)-size]
		}
		if n := base + suffix; utf8.RuneCountInString(n) <= 120 && !taken(n) {
			return n
		}
		if i > 1000 {
			return uuid.NewString()
		}
	}
}

func moveBudgets(ctx context.Context, tx *sql.Tx, schema string, renames map[string]string, rep *MergeReport) error {
	type budget struct {
		id                uuid.UUID
		principal, window string
		softUSD, hardUSD  sql.NullFloat64
		softTok, hardTok  sql.NullInt64
	}
	q := `SELECT id, principal_id, time_window, soft_usd, hard_usd, soft_tokens, hard_tokens FROM ` + schema + `.budgets WHERE scope = 'user' ORDER BY created_at, id` //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	var all []budget
	for rows.Next() {
		var b budget
		if err := rows.Scan(&b.id, &b.principal, &b.window, &b.softUSD, &b.hardUSD, &b.softTok, &b.hardTok); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, b)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// held is the row that owns each (principal, window) key after the move.
	held := map[string]*budget{}
	key := func(p, w string) string { return p + "\x00" + w }
	for i := range all {
		if _, moves := renames[all[i].principal]; !moves {
			held[key(all[i].principal, all[i].window)] = &all[i]
		}
	}
	for i := range all {
		b := &all[i]
		to, moves := renames[b.principal]
		if !moves {
			continue
		}
		into, collides := held[key(to, b.window)]
		if !collides {
			if _, err := tx.ExecContext(ctx, `UPDATE `+schema+`.budgets SET principal_id = $2, updated_at = now() WHERE id = $1`, b.id, to); err != nil { //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
				return err
			}
			b.principal = to
			held[key(to, b.window)] = b
			rep.RewrittenPrincipals++
			continue
		}
		into.softUSD, into.hardUSD = stricterF(into.softUSD, b.softUSD), stricterF(into.hardUSD, b.hardUSD)
		into.softTok, into.hardTok = stricterI(into.softTok, b.softTok), stricterI(into.hardTok, b.hardTok)
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+schema+`.budgets WHERE id = $1`, b.id); err != nil { //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+schema+`.budgets SET soft_usd = $2, hard_usd = $3, soft_tokens = $4, hard_tokens = $5, updated_at = now() WHERE id = $1`, //nolint:gosec // G202: schema passed plainIdentifier; table/column names are fixed
			into.id, into.softUSD, into.hardUSD, into.softTok, into.hardTok); err != nil {
			return err
		}
		rep.MergedBudgets = append(rep.MergedBudgets, BudgetMerge{Principal: to, Window: b.window, From: b.principal})
	}
	return nil
}

// stricterF is the stricter of two optional bounds: a configured one beats
// none, and of two the lower wins.
func stricterF(a, b sql.NullFloat64) sql.NullFloat64 {
	switch {
	case !a.Valid:
		return b
	case !b.Valid || a.Float64 <= b.Float64:
		return a
	default:
		return b
	}
}

func stricterI(a, b sql.NullInt64) sql.NullInt64 {
	switch {
	case !a.Valid:
		return b
	case !b.Valid || a.Int64 <= b.Int64:
		return a
	default:
		return b
	}
}
