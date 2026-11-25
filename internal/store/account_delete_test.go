package store

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestReprovisionedAccountStartsEmpty is the #563 contract seen from the
// next holder of an address: an account deleted and then created again under
// the same email starts with none of the first account's consent, browsers,
// share grants, central sign-in sessions or private prompts. Workspace-shared
// prompts the first account wrote stay readable by everyone but are no longer
// owned by the address.
func TestReprovisionedAccountStartsEmpty(t *testing.T) {
	t.Parallel()

	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const gone, mate = "leaving@x.com", "staying@x.com"
	const issuer, subject = "https://auth.example", "subject-1"
	for _, email := range []string{gone, mate} {
		if _, err := s.CreateUser(ctx, email, "password123"); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}

	if err := s.AcceptAutonomousPolicy(ctx, gone, 0); err != nil {
		t.Fatalf("AcceptAutonomousPolicy: %v", err)
	}
	if err := s.UpsertPushSubscription(ctx, gone, "https://push.example/browser-1", "auth", "p256dh"); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(mate))
	if err != nil {
		t.Fatalf("CreateRemoteMCPServer: %v", err)
	}
	if err := s.ShareRemoteMCPServer(ctx, mate, srv.ID, gone); err != nil {
		t.Fatalf("ShareRemoteMCPServer: %v", err)
	}
	if _, err := s.ExternalSessionEpoch(ctx, issuer, subject, gone); err != nil {
		t.Fatalf("ExternalSessionEpoch: %v", err)
	}
	const privatePrompt, sharedPrompt = "00000000-0000-0000-0000-00000000a001", "00000000-0000-0000-0000-00000000a002"
	for _, p := range []struct{ id, visibility string }{{privatePrompt, "private"}, {sharedPrompt, "workspace"}} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES ($1, $2, $3, 'c', $3)`,
			p.id, gone, p.visibility); err != nil {
			t.Fatalf("seed %s prompt: %v", p.visibility, err)
		}
	}

	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.CreateUser(ctx, gone, "another-password"); err != nil {
		t.Fatalf("CreateUser again: %v", err)
	}

	if _, accepted, err := s.GetAutonomousAcceptance(ctx, gone); err != nil || accepted {
		t.Errorf("Autonomous-mode acceptance after re-provisioning: accepted=%v err=%v, want not accepted", accepted, err)
	}
	if subs, err := s.ListPushSubscriptions(ctx, gone); err != nil || len(subs) != 0 {
		t.Errorf("push subscriptions after re-provisioning: %d (err %v), want 0", len(subs), err)
	}
	if shared, err := s.ListRemoteMCPServersSharedWith(ctx, gone); err != nil || len(shared) != 0 {
		t.Errorf("connectors shared with the address after re-provisioning: %d (err %v), want 0", len(shared), err)
	}
	if _, err := s.GetRemoteMCPServerForUse(ctx, gone, srv.ID); err == nil {
		t.Error("the re-provisioned account can use a connector shared with the deleted one")
	}
	if epoch, err := s.LookupExternalSessionEpoch(ctx, issuer, subject); err != nil || epoch != "" {
		t.Errorf("central sign-in session generation after deletion: %q (err %v), want none", epoch, err)
	}
	var owned int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sched.prompt_library WHERE owner_username = $1`, gone).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 0 {
		t.Errorf("%d prompts still owned by the address, want 0", owned)
	}
	var privateLeft, sharedLeft int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE id = $1), count(*) FILTER (WHERE id = $2) FROM sched.prompt_library`,
		privatePrompt, sharedPrompt).Scan(&privateLeft, &sharedLeft); err != nil {
		t.Fatal(err)
	}
	if privateLeft != 0 || sharedLeft != 1 {
		t.Errorf("prompts left: private %d (want 0), workspace-shared %d (want 1)", privateLeft, sharedLeft)
	}
}

// accountColumnPattern picks the columns that can name an account: an email,
// a user, an owner, a grantee, an author (…_by, author), an actor, a
// principal, a subject, a sender or recipient, an assignee or invitee, a
// participant or exclusion list, an account. It is deliberately wide; the
// false positives are listed below with why they are not an account.
const accountColumnPattern = `(email|user|owner|grantee|_by$|_by_|author|actor|principal|subject|sender|recipient|assignee|invitee|participant|excluded|account|member)`

// accountColumnRule is what DeleteUser does with one column that can name an
// account.
type accountColumnRule struct {
	// kind is "deleted" (DeleteUser removes, or a cascade removes, every row
	// naming the account), "scrubbed" (DeleteUser removes the account from a
	// list held in the column) or "kept" (left on purpose).
	kind string
	// why says, for a kept column, why it stays. Required for "kept".
	why string
	// by names, for a deleted column that does not hold the address itself,
	// the column of the same table that does: its rows go with that one, and
	// the row checks count them by it.
	by string
	// seed inserts one row naming the account ($1) for a deleted or scrubbed
	// column, so TestDeleteUserLeavesNoRowNamingTheAccount can check it is
	// gone afterwards. $2 is a teammate who stays. Seeds run in map order
	// after accountSeedParents.
	seed string
}

// accountSeedParents are the rows the seeds hang off: the account's own chat,
// a teammate's chat and a teammate's connector, so a seed can put the
// account's rows where its own deletion cascade does not reach.
const accountSeedParents = `
INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ($1, 'x', 1, 1), ($2, 'x', 1, 1);
INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('guard-own', $1, 't', 1, 1), ('guard-mate', $2, 't', 1, 1);
INSERT INTO chat.remote_mcp_servers (id, user_email, name, url, transport, status, created_at, updated_at)
  VALUES ('guard-own-srv', $1, 'own', 'https://own.example', 'streamable_http', 'connected', 1, 1),
         ('guard-mate-srv', $2, 'mate', 'https://mate.example', 'streamable_http', 'connected', 1, 1);
INSERT INTO chat.memories (id, user_email, content, created_at, updated_at) VALUES ('guard-mem', $1, 'm', 1, 1), ('guard-mate-mem', $2, 'm', 1, 1);
INSERT INTO chat.projects (id, owner_email, name, created_at, updated_at) VALUES ('guard-proj', $1, 'p', 1, 1);
`

// accountColumns is the reviewed list: every column accountColumnPattern
// matches must be here, with what account deletion does to it. A new table
// or column that names an account fails TestEveryAccountColumnHasADeletionRule
// until it is added — with a seed DeleteUser is shown to remove, or a reason
// it is kept.
var accountColumns = map[string]accountColumnRule{
	// ── deleted ──
	"chat.approvals.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.approvals (id, conversation_id, user_email, tool_name, args_json, created_at) VALUES ('guard-appr', 'guard-mate', $1, 't', '{}', 1)`},
	"chat.autonomous_acceptances.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.autonomous_acceptances (user_email, policy_version, accepted_at) VALUES ($1, 1, 1)`},
	"chat.chat_input_queue.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.chat_input_queue (id, conversation_id, user_email, client_input_id, message, mode, state, position, created_at, updated_at)
		       VALUES ('guard-in', 'guard-mate', $1, 'c', 'm', 'queued', 'queued', 1, 1, 1)`},
	"chat.conversation_order.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.conversation_order (user_email, scope, conversation_ids, updated_at) VALUES ($1, 'recent', '[]', 1)`},
	"chat.conversations.user_email": {kind: "deleted"}, // accountSeedParents: guard-own
	"chat.memories.user_email":      {kind: "deleted"}, // accountSeedParents: guard-mem
	"chat.memory_entities.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.memory_entities (id, user_email, name, name_norm, created_at) VALUES ('guard-ent', $1, 'n', 'n', 1)`},
	"chat.memory_relations.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.memory_entities (id, user_email, name, name_norm, created_at) VALUES ('guard-ent-mate', $2, 'n', 'n', 1);
		       INSERT INTO chat.memory_relations (id, user_email, memory_id, subject_entity_id, predicate, object_value, created_at)
		       VALUES ('guard-rel', $1, 'guard-mate-mem', 'guard-ent-mate', 'p', 'v', 1)`},
	"chat.project_user_state.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.projects (id, owner_email, name, created_at, updated_at) VALUES ('guard-mate-proj', $2, 'p', 1, 1);
		       INSERT INTO chat.project_user_state (project_id, user_email, updated_at) VALUES ('guard-mate-proj', $1, 1)`},
	"chat.projects.owner_email": {kind: "deleted"}, // accountSeedParents: guard-proj
	"chat.push_subscriptions.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.push_subscriptions (id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at) VALUES ('guard-push', $1, 'https://push.example/1', 'a', 'p', 1, 1)`},
	"chat.remote_mcp_oauth_flow.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.remote_mcp_oauth_flow (state, server_id, user_email, code_verifier_enc, created_at, expires_at) VALUES ('guard-flow', 'guard-mate-srv', $1, '\x00', 1, 9999999999)`},
	"chat.remote_mcp_servers.user_email": {kind: "deleted"}, // accountSeedParents: guard-own-srv
	"chat.remote_mcp_shares.grantee": {kind: "deleted",
		seed: `INSERT INTO chat.remote_mcp_shares (server_id, grantee, created_at) VALUES ('guard-mate-srv', $1, 1)`},
	"chat.shared_conversation_pins.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.shared_conversation_pins (user_email, conversation_id, pinned_at) VALUES ($1, 'guard-mate', 1)`},
	"chat.sidebar_layouts.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.sidebar_layouts (user_email, layout, updated_at) VALUES ($1, '{}', 1)`},
	"chat.ui_autosubmits.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.ui_autosubmits (id, conversation_id, user_email, call_id, action_id, values_json, deadline_ms, created_at_ms, updated_at_ms)
		       VALUES ('guard-auto', 'guard-mate', $1, 'c', 'a', '{}', 1, 1, 1)`},
	"chat.user_connector_prefs.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.user_connector_prefs (user_email, connector_kind, connector_id, enabled, updated_at) VALUES ($1, 'remote', 'x', TRUE, 1)`},
	"chat.user_personas.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.user_personas (id, user_email, name, display_name, instructions, created_at, updated_at) VALUES ('guard-persona', $1, 'guard', 'Guard', 'i', 1, 1)`},
	"chat.user_skills.user_email": {kind: "deleted",
		seed: `INSERT INTO chat.user_skills (id, user_email, name, description, body, created_at, updated_at) VALUES ('guard-skill', $1, 'guard', 'd', 'b', 1, 1)`},
	"core.external_auth_epochs.email": {kind: "deleted",
		seed: `INSERT INTO core.external_auth_epochs (issuer, subject, email, epoch, created_at, updated_at) VALUES ('https://auth.example', 'guard-sub', $1, 'e', 1, 1)`},
	"core.external_auth_epochs.subject": {kind: "deleted", by: "email"},
	"core.users.email":                  {kind: "deleted"}, // accountSeedParents
	"sched.chat_schedules.user_email": {kind: "deleted",
		seed: `INSERT INTO sched.chat_schedules (id, conversation_id, user_email, prompt, cron_expr, status, next_run_at, created_at, updated_at)
		       VALUES ('guard-sched', 'guard-mate', $1, 'p', '0 9 * * *', 'active', 1, 1, 1)`},
	// Private prompts are deleted; workspace-shared ones stay, owned by no
	// account (models.ReleasedPromptOwnerPrefix). Either way the address owns none.
	"sched.prompt_library.owner_username": {kind: "deleted",
		seed: `INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES
		       ('00000000-0000-0000-0000-0000000000a1', $1, 'mine', 'c', 'private'),
		       ('00000000-0000-0000-0000-0000000000a2', $1, 'ours', 'c', 'workspace')`},

	// ── scrubbed ── (DeleteUser's multiplayer scrub)
	"chat.conversations.multiplayer_participants": {kind: "scrubbed",
		seed: `UPDATE chat.conversations SET multiplayer_participants = jsonb_build_array($1::text, $2::text)::text WHERE id = 'guard-mate'`},
	"chat.conversations.multiplayer_excluded": {kind: "scrubbed",
		seed: `UPDATE chat.conversations SET multiplayer_excluded = jsonb_build_array($1::text)::text WHERE id = 'guard-mate'`},
	"chat.conversations.mcp_accounts": {kind: "scrubbed",
		seed: `UPDATE chat.conversations SET mcp_accounts = jsonb_build_object('` + multiplayerMCPAccountPrefix + `' || $1::text || ':srv', 'acct') WHERE id = 'guard-mate'`},

	// ── kept ──
	"chat.admin_actions.actor":                            {kind: "kept", why: "the chat-administration audit log records which administrator acted; nothing reads it as an account's state"},
	"chat.admin_requests.actor":                           {kind: "kept", why: "who asked for a chat-administration request: a pending one is cancelled when the account is deleted (deleteAccountKeyedRowsTx); finished and running ones stay as history"},
	"chat.admin_notes.author":                             {kind: "kept", why: "who wrote an administrator's note pinned on a chat: a label on a row that goes with its conversation (ON DELETE CASCADE)"},
	"chat.admin_notes.seen_by":                            {kind: "kept", why: "who last saw an administrator's note: a label on a row that goes with its conversation (ON DELETE CASCADE)"},
	"chat.approvals.staged_by_resume":                     {kind: "kept", why: "not an account: whether a resume turn staged the card"},
	"chat.chat_input_queue.sender_email":                  {kind: "kept", why: "the administrator whose words an admin-mode message carries: a queued one is cancelled when the account is deleted (deleteAccountKeyedRowsTx); the row belongs to the chat's owner (user_email), and one already run stays as the chat's history"},
	"chat.shared_files.updated_by":                        {kind: "kept", why: "who last wrote a file of the workspace's shared library: a label that grants nothing"},
	"chat.account_events.actor":                           {kind: "kept", why: "the account-events outbox records what happened and who did it; nothing reads it as an account's state"},
	"chat.account_events.email":                           {kind: "kept", why: "the account-events outbox (legacy/chat/067) records what happened; nothing reads it as an account's state"},
	"chat.approvals.mcp_account":                          {kind: "kept", why: "a connector account label inside an approval row, which goes with its user_email"},
	"chat.conversation_branch_origins.source_owner_email": {kind: "kept", why: "provenance shown on another person's branch of a chat; it grants nothing"},
	"chat.escalation_events.user_email":                   {kind: "kept", why: "a per-turn signal in a chat's history: it goes with its conversation (ON DELETE CASCADE); in a teammate's chat it is that chat's history"},
	"chat.memories.retired_by":                            {kind: "kept", why: "a label on a memory row, which goes with its user_email"},
	"chat.memory_relations.subject_entity_id":             {kind: "kept", why: "not an account: an entity id"},
	"chat.notify_settings.email_to":                       {kind: "kept", why: "the workspace's notification recipients, set by an admin; not an account"},
	"chat.notify_settings.owner_digest_enabled":           {kind: "kept", why: "not an account: a workspace setting"},
	"chat.notify_settings.owner_digest_hour_utc":          {kind: "kept", why: "not an account: a workspace setting"},
	"chat.notify_settings.smtp_username":                  {kind: "kept", why: "not an account: the SMTP login of the workspace's mail relay"},
	"chat.notify_settings.updated_by":                     {kind: "kept", why: "who last changed a workspace setting: a label that grants nothing"},
	"chat.remote_mcp_servers.account":                     {kind: "kept", why: "not an account here: the account at the remote service, inside a row that goes with its user_email"},
	"chat.remote_mcp_servers.authorization_endpoint":      {kind: "kept", why: "not an account: the remote service's authorization URL, inside a row that goes with its user_email"},
	"chat.shared_files.uploaded_by":                       {kind: "kept", why: "the team file library belongs to the workspace; the uploader is a label that grants nothing"},
	"chat.tool_call_log.user_email":                       {kind: "kept", why: "a chat's tool-call history: it goes with its conversation (ON DELETE CASCADE); in a teammate's chat it is that chat's history"},
	"chat.turn_metrics.user_email":                        {kind: "kept", why: "usage is an accounting record and outlives the conversation and the account (legacy/chat/038)"},
	"chat.ui_autosubmits.baseline_user_msg_id":            {kind: "kept", why: "not an account: a message id"},
	"chat.user_connector_prefs.default_account":           {kind: "kept", why: "not an account here: a connector account label inside a row that goes with its user_email"},
	"chat.workspace_settings.updated_by":                  {kind: "kept", why: "who last changed a workspace setting: a label that grants nothing"},
	"chat.workspace_sidebar_layout.updated_by":            {kind: "kept", why: "who last changed the workspace sidebar: a label that grants nothing"},
	"core.external_access_state.email":                    {kind: "kept", why: "central Auth's last applied state for its (issuer, subject): the version kept here stops an older event from being applied again"},
	"core.external_access_state.subject":                  {kind: "kept", why: "central Auth's last applied state for its (issuer, subject): the version kept here stops an older event from being applied again"},
	"core.external_logout_events.subject":                 {kind: "kept", why: "the replay record of central sign-out tokens, pruned after a week"},
	"sched.agent_note_proposals.decided_by":               {kind: "kept", why: "authorship of a workspace note decision: a label that grants nothing"},
	"sched.agent_note_proposals.proposed_by":              {kind: "kept", why: "authorship of a workspace note proposal: a label that grants nothing"},
	"sched.agent_notes.created_by":                        {kind: "kept", why: "authorship of a workspace note: a label that grants nothing"},
	"sched.agent_notes.updated_by":                        {kind: "kept", why: "authorship of a workspace note: a label that grants nothing"},
	"sched.budgets.principal_id":                          {kind: "kept", why: "a spend limit an admin set; deleting it would lift the limit, so it stays until an admin removes it"},
	"sched.chat_schedules.lease_owner":                    {kind: "kept", why: "not an account: the scheduler process holding the lease"},
	"sched.datasets.created_by_key_id":                    {kind: "kept", why: "not an account: an API key id"},
	"sched.datasets.created_by_user_id":                   {kind: "kept", why: "the account's id (core.users.id), which a later account under the same email does not get; the dataset is workspace data"},
	"sched.task_feedback.created_by":                      {kind: "kept", why: "authorship of feedback on a workspace task: a label that grants nothing"},
	"sched.task_iterations.created_by":                    {kind: "kept", why: "spend accounting (migration 0210): the account's id (core.users.id), which a later account under the same email does not get; usage and budgets keep counting the spend under it, as turn_metrics outlives the account"},
	"sched.task_iterations.created_by_key_id":             {kind: "kept", why: "not an account: an API key id"},
	"sched.task_learned_instructions.activated_by":        {kind: "kept", why: "who activated a task's learned instructions: a label that grants nothing"},
	"sched.task_triggers.email_policy":                    {kind: "kept", why: "the sender addresses an email trigger accepts: addresses, not accounts"},
	"sched.tasks.created_by":                              {kind: "kept", why: "the account's id (core.users.id), which a later account under the same email does not get; Operations Center tasks are workspace data"},
	"sched.tasks.created_by_key_id":                       {kind: "kept", why: "not an account: an API key id"},
	"sched.tasks.created_by_task_id":                      {kind: "kept", why: "not an account: a task id"},
	"sched.tasks.lease_owner":                             {kind: "kept", why: "not an account: the worker holding the lease"},
	"sched.tasks.private_emails":                          {kind: "kept", why: "not an account: whether the task keeps its sent email out of the team Outbox"},
	"sched.trigger_events.sender":                         {kind: "kept", why: "the sender of an inbound trigger email: an address, not an account"},
	"sched.trigger_events.subject":                        {kind: "kept", why: "not an account: an inbound email's subject line"},
	"sched.users_premerge_m4.username":                    {kind: "kept", why: "the frozen pre-merge copy of the legacy scheduler users, read only by the one-time identity merge (docs/IDENTITY.md); nothing signs in from it"},
}

// TestEveryAccountColumnHasADeletionRule: every column that can name an
// account has a reviewed rule, and every rule names a column that exists.
func TestEveryAccountColumnHasADeletionRule(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT c.table_schema || '.' || c.table_name || '.' || c.column_name
		  FROM information_schema.columns c
		  JOIN information_schema.tables t USING (table_schema, table_name)
		 WHERE t.table_type = 'BASE TABLE'
		   AND c.table_schema IN ('chat', 'core', 'sched')
		   AND c.column_name ~ $1
		 ORDER BY 1`, accountColumnPattern)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		seen[col] = true
		rule, ok := accountColumns[col]
		switch {
		case !ok:
			t.Errorf("%s can name an account and has no rule: make DeleteUser delete or scrub it (with a seed in accountColumns), or list it as kept with the reason", col)
		case rule.kind == "kept" && rule.why == "":
			t.Errorf("%s is kept without a reason", col)
		case rule.kind != "kept" && rule.kind != "deleted" && rule.kind != "scrubbed":
			t.Errorf("%s: unknown rule %q", col, rule.kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for col := range accountColumns {
		if !seen[col] {
			t.Errorf("accountColumns lists %s, which no longer exists (or no longer matches the pattern)", col)
		}
	}
}

// TestDeleteUserLeavesNoRowNamingTheAccount seeds a row for every deleted or
// scrubbed column in accountColumns — in the account's own chat where it has
// one, in a teammate's chat or connector where the conversation cascade does
// not reach — deletes the account, and checks that no such column still names
// it while the teammate's rows stay.
func TestDeleteUserLeavesNoRowNamingTheAccount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const gone, mate = "gone@guard.test", "mate@guard.test"
	if _, err := s.db.ExecContext(ctx, accountSeedParentsSQL(accountSeedParents, gone, mate)); err != nil {
		t.Fatalf("seed parents: %v", err)
	}
	for col, rule := range accountColumns {
		if rule.seed == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, accountSeedParentsSQL(rule.seed, gone, mate)); err != nil {
			t.Fatalf("seed %s: %v", col, err)
		}
	}
	naming := func(col string, rule accountColumnRule) int {
		t.Helper()
		parts := strings.SplitN(col, ".", 3)
		table, column := parts[0]+"."+parts[1], parts[2]
		if rule.by != "" {
			column = rule.by
		}
		q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE lower(%s::text) = $1`, table, column)
		if rule.kind == "scrubbed" {
			q = fmt.Sprintf(`SELECT count(*) FROM %s WHERE strpos(lower(%s::text), $1) > 0`, table, column)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, q, gone).Scan(&n); err != nil {
			t.Fatalf("%s: %v", col, err)
		}
		return n
	}
	checked := func(rule accountColumnRule) bool { return rule.kind != "kept" }
	for col, rule := range accountColumns {
		if checked(rule) && naming(col, rule) == 0 {
			t.Fatalf("%s: the seed left no row naming the account, so the check below would prove nothing", col)
		}
	}
	// The address as an admin may type it: every statement must match the
	// normalized value the rows hold.
	if err := s.DeleteUser(ctx, "  Gone@Guard.TEST "); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for col, rule := range accountColumns {
		if !checked(rule) {
			continue
		}
		if n := naming(col, rule); n != 0 {
			t.Errorf("%s: %d rows still name the deleted account", col, n)
		}
	}
	var mateConv, mateParticipants int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE strpos(multiplayer_participants, $1) > 0)
		FROM chat.conversations WHERE id = 'guard-mate'`, mate).Scan(&mateConv, &mateParticipants); err != nil {
		t.Fatal(err)
	}
	if mateConv != 1 || mateParticipants != 1 {
		t.Errorf("the teammate's chat: %d rows, teammate still a participant %d; want 1 and 1", mateConv, mateParticipants)
	}
}

// accountSeedParentsSQL binds the two addresses into a multi-statement seed:
// pgx runs a parameterless multi-statement string as one simple query, so the
// $1 / $2 placeholders are replaced with quoted literals here (test-only
// constants, no caller input).
func accountSeedParentsSQL(sql, gone, mate string) string {
	q := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	return strings.NewReplacer("$1", q(gone), "$2", q(mate)).Replace(sql)
}

// seedAccount creates accounts for a test that writes rows only an existing
// account may have (every row DeleteUser removes by the address: lockAccountTx).
func seedAccount(t testing.TB, s *Store, emails ...string) {
	t.Helper()
	for _, email := range emails {
		if _, err := s.CreateUser(context.Background(), email, "seed-password-1"); err != nil && !errors.Is(err, ErrUserExists) {
			t.Fatalf("seed account %s: %v", email, err)
		}
	}
}

// The writes a turn makes for its user after it ends — memory, skill and
// persona proposals — write nothing once the account is gone.
func TestProposalsAfterAccountDeletionWriteNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const gone = "late@x.com"
	seedAccount(t, s, gone)
	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemoryProposal(ctx, gone, "conv-1", MemoryProposalParams{Content: "a fact", Origin: "auto"}); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("memory proposal: err = %v, want ErrUserNotFound", err)
	}
	if _, err := s.CreateUserSkillProposal(ctx, gone, "weekly", "a skill", "1. Do it."); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("skill proposal: err = %v, want ErrUserNotFound", err)
	}
	if _, err := s.ProposeUserPersona(ctx, gone, validPersonaInput("twin")); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("persona proposal: err = %v, want ErrUserNotFound", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM chat.memories WHERE user_email = $1) +
		(SELECT count(*) FROM chat.user_skills WHERE user_email = $1) +
		(SELECT count(*) FROM chat.user_personas WHERE user_email = $1)`, gone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows written for the deleted account", n)
	}
}

// A proposal written while the account is being deleted waits for the
// deletion and then writes nothing: no row survives the account whichever
// way the two meet. Not parallel: it uses DeleteUser's test seam.
func TestProposalRacingAccountDeletionLeavesNoRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const gone = "racing@x.com"
	seedAccount(t, s, gone)
	proposed := make(chan error, 1)
	deleteUserAfterGuard = func() {
		deleteUserAfterGuard = nil
		go func() {
			_, err := s.CreateMemoryProposal(ctx, gone, "conv-1", MemoryProposalParams{Content: "a late fact", Origin: "auto"})
			proposed <- err
		}()
		// Give the proposal every chance to land before the deletion goes on.
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { deleteUserAfterGuard = nil })
	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := <-proposed; !errors.Is(err, ErrUserNotFound) {
		t.Errorf("proposal racing the deletion: err = %v, want ErrUserNotFound", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.memories WHERE user_email = $1`, gone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d memories outlived the account", n)
	}
}

// The maintenance sweep removes rows naming an address with no account in
// the tables only a signed-in account writes, and nothing else: rows of a
// live account, prompts owned by an API key, the admin key or no account,
// prompts the identity merge quarantined under a username that is not in
// normalized form, rows of a chat a webhook started for an address with no
// account, a central sign-in generation a live session may still carry, and
// share grants to an address that has no account yet stay.
func TestSweepDeletedAccountRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const live, gone = "live@x.com", "gone@x.com"
	seedAccount(t, s, live)
	old, fresh := time.Now().Add(-48*time.Hour).Unix(), time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, accountSeedParentsSQL(`
		INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('c-live', $1, 't', 1, 1);
		INSERT INTO chat.remote_mcp_servers (id, user_email, name, url, transport, status, created_at, updated_at)
		  VALUES ('srv-live', $1, 'n', 'https://x.example', 'streamable_http', 'connected', 1, 1);
		INSERT INTO chat.autonomous_acceptances (user_email, policy_version, accepted_at) VALUES ($1, 1, 1), ($2, 1, 1);
		INSERT INTO chat.push_subscriptions (id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at)
		  VALUES ('p1', $1, 'https://push.example/1', 'a', 'k', 1, 1), ('p2', $2, 'https://push.example/2', 'a', 'k', 1, 1);
		INSERT INTO chat.ui_autosubmits (id, conversation_id, user_email, call_id, action_id, values_json, deadline_ms, created_at_ms, updated_at_ms)
		  VALUES ('u1', 'c-live', $1, 'c1', 'a', '{}', 1, 1, 1), ('u2', 'c-live', $2, 'c2', 'a', '{}', 1, 1, 1);
		INSERT INTO chat.memory_entities (id, user_email, name, name_norm, created_at) VALUES ('e1', $1, 'n', 'n', 1), ('e2', $2, 'n', 'n', 1);
		INSERT INTO core.external_auth_epochs (issuer, subject, email, epoch, created_at, updated_at) VALUES
		  ('https://auth.example', 's-live', $1, 'e', 1, 1),
		  ('https://auth.example', 's-gone-old', $2, 'e', 1, `+fmt.Sprint(old)+`),
		  ('https://auth.example', 's-gone-fresh', $2, 'e', 1, `+fmt.Sprint(fresh)+`);
		INSERT INTO chat.remote_mcp_shares (server_id, grantee, created_at) VALUES ('srv-live', $2, 1);
		INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES
		  ('00000000-0000-0000-0000-0000000000b1', $1, 'live', 'c', 'private'),
		  ('00000000-0000-0000-0000-0000000000b2', $2, 'gone', 'c', 'private'),
		  ('00000000-0000-0000-0000-0000000000b3', 'apikey:key_1', 'key', 'c', 'private'),
		  ('00000000-0000-0000-0000-0000000000b4', 'admin', 'admin', 'c', 'private'),
		  ('00000000-0000-0000-0000-0000000000b5', 'Deleted account 00000000-0000-0000-0000-0000000000b5', 'released', 'c', 'workspace'),
		  ('00000000-0000-0000-0000-0000000000b6', $2, 'gone shared', 'c', 'workspace'),
		  ('00000000-0000-0000-0000-0000000000b7', 'Brad@X.com', 'quarantined', 'c', 'private'),
		  ('00000000-0000-0000-0000-0000000000b8', ' brad@x.com', 'quarantined too', 'c', 'private');
		INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('c-hook', 'hook@x.com', 't', 1, 1), ('c-hook2', 'Hook2@X.com', 't', 1, 1);
		INSERT INTO chat.chat_input_queue (id, conversation_id, user_email, client_input_id, message, mode, state, position, created_at, updated_at)
		  VALUES ('q-hook2', 'c-hook2', 'hook2@x.com', 'q3', 'm', 'queued', 'queued', 1, 1, 1);
		INSERT INTO chat.push_subscriptions (id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at)
		  VALUES ('p-tab', E'gone@x.com\t', 'https://push.example/tab', 'a', 'k', 1, 1),
		         ('p-nbsp', E'gone@x.com\u00a0', 'https://push.example/nbsp', 'a', 'k', 1, 1);
		INSERT INTO chat.chat_input_queue (id, conversation_id, user_email, client_input_id, message, mode, state, position, created_at, updated_at)
		  VALUES ('q-gone', 'c-live', $2, 'q1', 'm', 'queued', 'queued', 1, 1, 1),
		         ('q-hook', 'c-hook', 'hook@x.com', 'q2', 'm', 'queued', 'queued', 1, 1, 1);
		INSERT INTO chat.approvals (id, conversation_id, user_email, tool_name, args_json, created_at)
		  VALUES ('a-gone', 'c-live', $2, 't', '{}', 1), ('a-hook', 'c-hook', 'hook@x.com', 't', '{}', 1);
		INSERT INTO sched.chat_schedules (id, conversation_id, user_email, prompt, cron_expr, status, next_run_at, created_at, updated_at)
		  VALUES ('s-gone', 'c-live', $2, 'p', '0 9 * * *', 'active', 1, 1, 1),
		         ('s-hook', 'c-hook', 'hook@x.com', 'p', '0 9 * * *', 'active', 1, 1, 1);
	`, live, gone)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.SweepDeletedAccountRows(ctx); err != nil {
		t.Fatalf("SweepDeletedAccountRows: %v", err)
	}
	for _, c := range []struct {
		name, q string
		want    int
	}{
		{"acceptances of the deleted address", `SELECT count(*) FROM chat.autonomous_acceptances WHERE user_email = 'gone@x.com'`, 0},
		{"acceptances of the live account", `SELECT count(*) FROM chat.autonomous_acceptances WHERE user_email = 'live@x.com'`, 1},
		{"push subscriptions left (the live account's, and two stored with a tab or no-break space the normalizer would trim)", `SELECT count(*) FROM chat.push_subscriptions`, 3},
		{"a row of a chat whose webhook owner is stored as configured (Hook2@X.com)", `SELECT count(*) FROM chat.chat_input_queue WHERE id = 'q-hook2'`, 1},
		{"countdowns left", `SELECT count(*) FROM chat.ui_autosubmits`, 1},
		{"memory entities left", `SELECT count(*) FROM chat.memory_entities`, 1},
		{"sign-in generations left (live account, and one a live session may carry)", `SELECT count(*) FROM core.external_auth_epochs WHERE subject IN ('s-live', 's-gone-fresh')`, 2},
		{"an old sign-in generation of the deleted address", `SELECT count(*) FROM core.external_auth_epochs WHERE subject = 's-gone-old'`, 0},
		{"share grant to an address with no account yet", `SELECT count(*) FROM chat.remote_mcp_shares`, 1},
		{"private prompt of the deleted address", `SELECT count(*) FROM sched.prompt_library WHERE name = 'gone'`, 0},
		{"prompts kept (live, key, admin, released, workspace-shared, two the identity merge quarantined)", `SELECT count(*) FROM sched.prompt_library WHERE name <> 'gone'`, 7},
		{"a deleted participant's queued message, approval and schedule in a teammate's chat", `SELECT
			(SELECT count(*) FROM chat.chat_input_queue WHERE user_email = 'gone@x.com') +
			(SELECT count(*) FROM chat.approvals WHERE user_email = 'gone@x.com') +
			(SELECT count(*) FROM sched.chat_schedules WHERE user_email = 'gone@x.com')`, 0},
		{"rows of a chat a webhook started for an address with no account", `SELECT
			(SELECT count(*) FROM chat.chat_input_queue WHERE user_email = 'hook@x.com') +
			(SELECT count(*) FROM chat.approvals WHERE user_email = 'hook@x.com') +
			(SELECT count(*) FROM sched.chat_schedules WHERE user_email = 'hook@x.com')`, 3},
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, c.q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != c.want {
			t.Errorf("%s: %d, want %d", c.name, n, c.want)
		}
	}
}

// DeleteUserPreflight answers what DeleteUser would, changing nothing.
func TestDeleteUserPreflight(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.DeleteUserPreflight(ctx, "nobody@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("no account: err = %v, want ErrUserNotFound", err)
	}
	seedAccount(t, s, "here@x.com")
	if err := s.DeleteUserPreflight(ctx, " Here@X.com "); err != nil {
		t.Errorf("an account that can go: %v", err)
	}
	if _, err := s.GetUser(ctx, "here@x.com"); err != nil {
		t.Errorf("the preflight changed the account: %v", err)
	}
}

// AccountDeletionSummary counts what DeleteUser would remove — the account's
// conversations, owned projects, its memories and its projects' shared ones —
// and changes nothing.
func TestAccountDeletionSummary(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AccountDeletionSummary(ctx, "nobody@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("no account: err = %v, want ErrUserNotFound", err)
	}
	const owner, other = "owner@x.com", "other@x.com"
	seedAccount(t, s, owner, other)
	for i := 0; i < 2; i++ {
		if _, err := s.CreateConversation(ctx, owner, "mine", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateConversation(ctx, other, "theirs", "", "", false); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, &Project{OwnerEmail: owner, Name: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemory(ctx, owner, "a personal memory", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProjectMemory(ctx, p.ID, other, "a shared project memory", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemory(ctx, other, "someone else's memory", "manual", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.AccountDeletionSummary(ctx, " Owner@X.com ")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AccountForDeletion(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	want := AccountDeletionSummary{ID: id, Email: owner, Role: got.Role, Conversations: 2, OwnedProjects: 1, Memories: 2}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if got.Role == "" {
		t.Error("the summary does not carry the account's role")
	}
	if _, err := s.GetUser(ctx, owner); err != nil {
		t.Errorf("the summary changed the account: %v", err)
	}
}

// ServerHeartbeatSeen sees a running server's heartbeat and none once it has
// stopped.
func TestServerHeartbeatSeen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if seen, err := s.ServerHeartbeatSeen(ctx); err != nil || seen {
		t.Fatalf("no server: seen %v, err %v", seen, err)
	}
	release, err := s.HoldServerPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seen, err := s.ServerHeartbeatSeen(ctx); err != nil || !seen {
		t.Errorf("a server running: seen %v, err %v", seen, err)
	}
	release()
	if seen, err := s.ServerHeartbeatSeen(ctx); err != nil || seen {
		t.Errorf("after the server stopped: seen %v, err %v", seen, err)
	}
}

// A participant's write in a teammate's chat that meets the participant's
// deletion waits for it and is then refused: it takes the account row before
// the chat's, in DeleteUser's order. Not parallel: it uses DeleteUser's seam.
func TestParticipantWriteRacingAccountDeletionIsRefused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const gone, mate = "participant@x.com", "owner@x.com"
	seedAccount(t, s, gone)
	seedAccount(t, s, mate)
	conv, err := s.CreateConversation(ctx, mate, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// A Multiplayer chat with no team stamp: anyone on the deployment may
	// take part, so the participant is in no list the deletion rewrites.
	if _, err := s.db.ExecContext(ctx, `UPDATE chat.conversations SET multiplayer = TRUE WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	created := make(chan error, 1)
	deleteUserAfterGuard = func() {
		deleteUserAfterGuard = nil
		go func() {
			_, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: gone, Prompt: "p", CronExpr: "0 9 * * *", AllowMultiple: true})
			created <- err
		}()
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { deleteUserAfterGuard = nil })
	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := <-created; err == nil {
		t.Fatal("a schedule was created for the participant while its account was being deleted")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sched.chat_schedules WHERE user_email = $1`, gone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d schedules outlived the participant's account", n)
	}
}

// A central sign-out delivered again (same event id) after the account's
// session generation was removed with the account has nothing to rotate: it
// succeeds instead of failing.
func TestReplayedSignOutAfterDeletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const email, issuer, subject = "signout@x.com", "https://auth.example", "sub-signout"
	seedAccount(t, s, email)
	if _, _, err := s.RevokeExternalSessions(ctx, "jti-1", issuer, subject, email); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := s.DeleteUser(ctx, email); err != nil {
		t.Fatal(err)
	}
	epoch, applied, err := s.RevokeExternalSessions(ctx, "jti-1", issuer, subject, email)
	if err != nil || applied || epoch != "" {
		t.Fatalf("replay after deletion = %q, %v, %v; want nothing to rotate, no error", epoch, applied, err)
	}
}

// Deleting an administrator cancels the chat-administration work they asked
// for that no server has picked up — a pending request fails with a fixed
// reason, a queued admin-mode message is cancelled — and keeps the rest as
// history: a finished request, one a server is carrying out, the audit log
// and their notes on chats.
func TestDeleteUserCancelsPendingAdminWork(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const boss, owner = "boss@x.com", "owner@x.com"
	seedAccount(t, s, boss)
	seedAccount(t, s, owner)
	conv, err := s.CreateConversation(ctx, owner, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	actor := AdminActor{Email: " Boss@X.com ", Via: AdminViaCLI}
	request := func(kind, payload string) AdminRequest {
		t.Helper()
		r, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: actor, Kind: kind, ConversationID: conv.ID, Payload: payload, Reason: "testing"})
		if err != nil {
			t.Fatalf("CreateAdminRequest(%s): %v", kind, err)
		}
		return r
	}
	pending := request(AdminRequestMessage, "still waiting")
	queued := request(AdminRequestMessage, "queued for the owner")
	row, ok, err := s.QueueAdminMessage(ctx, queued.ID)
	if err != nil || !ok {
		t.Fatalf("QueueAdminMessage = %v, %v", ok, err)
	}
	finished := request(AdminRequestStop, "")
	if ok, err := s.FinishAdminRequest(ctx, finished.ID, AdminRequestDone, "stopped"); err != nil || !ok {
		t.Fatalf("FinishAdminRequest = %v, %v", ok, err)
	}
	running := request(AdminRequestArchive, "archive")
	if ok, err := s.ClaimAdminRequest(ctx, running.ID); err != nil || !ok {
		t.Fatalf("ClaimAdminRequest = %v, %v", ok, err)
	}
	if _, err := s.AdminSetNote(ctx, actor, conv.ID, "check on this chat"); err != nil {
		t.Fatalf("AdminSetNote: %v", err)
	}
	var actionsBefore int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.admin_actions WHERE actor = $1`, boss).Scan(&actionsBefore); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, boss); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	for _, c := range []struct {
		name, id, state, result string
	}{
		{"the pending request", pending.ID, AdminRequestFailed, adminRequestActorDeleted},
		{"the request already queued", queued.ID, AdminRequestDone, "queued"},
		{"the finished request", finished.ID, AdminRequestDone, "stopped"},
		{"the request a server is carrying out", running.ID, AdminRequestRunning, ""},
	} {
		got, err := s.GetAdminRequest(ctx, c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.State != c.state || got.Result != c.result {
			t.Errorf("%s: state %q result %q, want %q %q", c.name, got.State, got.Result, c.state, c.result)
		}
	}
	if got := inputByID(t, s, conv.ID, row.ID); got.State != InputStateCancelled {
		t.Errorf("the queued admin-mode message is %q, want cancelled", got.State)
	}
	if note, err := s.GetAdminNote(ctx, conv.ID); err != nil || note == nil {
		t.Errorf("the administrator's note on the chat: %v, %v; want kept", note, err)
	}
	var actionsAfter int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.admin_actions WHERE actor = $1`, boss).Scan(&actionsAfter); err != nil {
		t.Fatal(err)
	}
	if actionsBefore == 0 || actionsAfter != actionsBefore {
		t.Errorf("audit log entries %d before, %d after; want them kept", actionsBefore, actionsAfter)
	}
}

// A queued message and an approval written for a participant while the
// participant's account is being deleted wait for the deletion and are then
// refused: both writers take the account row before the chat's. Not parallel:
// it uses DeleteUser's seam.
func TestParticipantInputAndApprovalRacingAccountDeletionAreRefused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const gone, mate = "late-participant@x.com", "chat-owner@x.com"
	seedAccount(t, s, gone)
	seedAccount(t, s, mate)
	conv, err := s.CreateConversation(ctx, mate, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE chat.conversations SET multiplayer = TRUE WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	enqueued, staged := make(chan error, 1), make(chan error, 1)
	deleteUserAfterGuard = func() {
		deleteUserAfterGuard = nil
		go func() {
			_, _, err := s.EnqueueInput(ctx, InputQueueRow{ID: "late-input", ConversationID: conv.ID, UserEmail: gone, ClientInputID: "late", Message: "m", Attachments: "[]", Mode: InputModeQueued})
			enqueued <- err
		}()
		go func() {
			_, err := s.CreateApproval(ctx, conv.ID, gone, "run_python", "call-1", "{}", 0, ApprovalSeat{})
			staged <- err
		}()
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { deleteUserAfterGuard = nil })
	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := <-enqueued; err == nil {
		t.Error("a message was queued for the participant while its account was being deleted")
	}
	if err := <-staged; err == nil {
		t.Error("an approval was staged for the participant while its account was being deleted")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM chat.chat_input_queue WHERE user_email = $1) +
		(SELECT count(*) FROM chat.approvals WHERE user_email = $1)`, gone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows outlived the participant's account", n)
	}
	// The owner's own writes, and a webhook chat's owner with no account,
	// are still accepted.
	if _, _, err := s.EnqueueInput(ctx, InputQueueRow{ID: "owner-input", ConversationID: conv.ID, UserEmail: mate, ClientInputID: "own", Message: "m", Attachments: "[]", Mode: InputModeQueued}); err != nil {
		t.Errorf("the owner's input: %v", err)
	}
	// As CreateWebhookConversation files it: the owner needs no account.
	hook, err := insertConversation(ctx, s.db, newConversationID(ctx), "hook@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApproval(ctx, hook.ID, "hook@x.com", "run_python", "call-2", "{}", 0, ApprovalSeat{}); err != nil {
		t.Errorf("an approval in a chat whose owner has no account: %v", err)
	}
}

// A server queueing an administrator's message while the administrator is
// being deleted: both finish, and the message ends cancelled whichever came
// first — the deletion takes the request rows before any chat, the order the
// queueing takes them in. Not parallel: it uses DeleteUser's seam.
func TestQueueAdminMessageRacingAdminDeletion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const boss, owner = "racing-boss@x.com", "racing-owner@x.com"
	seedAccount(t, s, boss)
	seedAccount(t, s, owner)
	conv, err := s.CreateConversation(ctx, owner, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: AdminActor{Email: boss, Via: AdminViaWeb}, Kind: AdminRequestMessage, ConversationID: conv.ID, Payload: "hello", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	deleteUserAfterGuard = func() {
		deleteUserAfterGuard = nil
		go func() {
			_, _, err := s.QueueAdminMessage(ctx, req.ID)
			queued <- err
		}()
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { deleteUserAfterGuard = nil })
	if err := s.DeleteUser(ctx, boss); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := <-queued; err != nil {
		t.Fatalf("QueueAdminMessage: %v", err)
	}
	got, err := s.GetAdminRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch got.State {
	case AdminRequestDone:
		if got.InputState != InputStateCancelled {
			t.Errorf("the message was queued first, but its queue row is %q, want cancelled", got.InputState)
		}
	case AdminRequestFailed:
		if got.InputID != "" {
			t.Errorf("the request was cancelled, but a queue row %s exists", got.InputID)
		}
	default:
		t.Errorf("request state %q", got.State)
	}
}

// The deletion takes an administrator's pending request rows before any chat
// row, the order a server queueing an admin message takes them in (request,
// then chat). Here a transaction holds a request row as QueueAdminMessage
// does, the deletion starts and waits for it, and the transaction then takes
// the chat — which the deletion would already hold, scrubbing the
// administrator from the chat's multiplayer list, if it took chats first: the
// two would deadlock.
func TestDeleteUserTakesAdminRequestsBeforeChats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const boss, owner = "order-boss@x.com", "order-owner@x.com"
	seedAccount(t, s, boss)
	seedAccount(t, s, owner)
	conv, err := s.CreateConversation(ctx, owner, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// The administrator takes part in the chat, so the deletion's multiplayer
	// scrub writes this chat's row.
	if _, err := s.db.ExecContext(ctx, `UPDATE chat.conversations SET multiplayer = TRUE, multiplayer_participants = $2 WHERE id = $1`,
		conv.ID, `["`+boss+`"]`); err != nil {
		t.Fatal(err)
	}
	req, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: AdminActor{Email: boss, Via: AdminViaWeb}, Kind: AdminRequestMessage, ConversationID: conv.ID, Payload: "hello", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM chat.admin_requests WHERE id = $1 FOR UPDATE`, req.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteUser(ctx, boss) }()
	time.Sleep(300 * time.Millisecond) // the deletion now waits for the request row
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM chat.conversations WHERE id = $1 FOR UPDATE`, conv.ID); err != nil {
		t.Fatalf("taking the chat after the request row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
}

// accountLockCalls take the account-row lock before a write: lockAccountTx
// and the helpers built on it, and the Operations Center's prompt-owner lock.
var accountLockCalls = map[string]bool{
	"lockAccountTx": true, "withAccountRow": true, "lockWriterAccountTx": true,
	"lockAndVerifyOwnedOrMultiplayerTx": true, "lockPromptOwnerTx": true,
}

// accountWriteHelpers insert into such a table against their caller's
// transaction: each caller is checked as if it held the INSERT.
var accountWriteHelpers = map[string]bool{"insertConversation": true, "insertInputTx": true}

// accountWriterExempt are the functions that insert into a table DeleteUser
// clears by the account's address (or call an accountWriteHelpers helper)
// without taking the account-row lock, keyed "<path>:<function>", each with
// the reason.
var accountWriterExempt = map[string]string{
	"internal/store/store.go:insertConversation":                     "a helper (accountWriteHelpers): its callers are checked",
	"internal/store/input_queue.go:insertInputTx":                    "a helper (accountWriteHelpers): its callers are checked",
	"internal/store/webhook_deliveries.go:CreateWebhookConversation": "a webhook's chat may belong to an address with no account",
	"internal/store/admin_chats.go:QueueAdminMessage":                "the message is the chat owner's and is written under the chat's row lock: deleting the owner deletes the chat",
	"internal/store/approval_resume.go:ClaimApprovalResume":          "writes under the chat's row lock, which DeleteUser takes first for every chat the account has approvals in (lockParticipantChatsTx)",
	"internal/store/autonomous.go:sweepOneUIAutoSubmit":              "writes under the chat's row lock, which DeleteUser takes first for every chat the account has countdowns in (lockParticipantChatsTx)",
	"internal/store/remote_mcp_shares.go:ShareRemoteMCPServer":       "a connector may be shared with an address before its account exists",
	"internal/store/users.go:ExternalSessionEpoch":                   "central sign-in, which may come before the person's account exists",
	"internal/store/users.go:RevokeExternalSessions":                 "a central sign-out: the generation it writes only ends sessions",
	"internal/store/access_provisioning.go:ApplyExternalAccess":      "provisions the account itself, in the same transaction",
	"internal/store/import.go:ImportConversation":                    "the one-time legacy import (docs/LEGACY-IMPORT.md) copies the source system's rows as they were",
	"internal/store/import.go:ImportMemory":                          "the one-time legacy import (docs/LEGACY-IMPORT.md) copies the source system's rows as they were",
}

// accountTablesWithForeignKey reference core.users(email), so an insert for a
// missing account fails on its own and waits on the same row lock.
var accountTablesWithForeignKey = map[string]bool{"shared_conversation_pins": true, "project_user_state": true}

// accountTableConstants are the Go constants an INSERT names its table by.
var accountTableConstants = map[string]string{"chatSchedulesTable": "chat_schedules"}

// TestAccountKeyedWritersTakeTheAccountLock: every function in cmd/ or
// internal/ that inserts into a table DeleteUser clears by the account's
// address (accountColumns' "deleted" rules) takes the account-row lock in the
// same function (accountLockCalls), or is listed in accountWriterExempt with
// the reason. Without the lock a write still in flight when the account is
// deleted — a turn, a request, a server running while the CLI deletes — could
// commit after the deletion and be found by a later account under the email.
//
// What it does not check: it looks for a lock call anywhere in the function,
// not that the lock comes first or runs in the same transaction as the
// insert (review that; lockAccountTx's doc says why it must); it follows the
// insert helpers in accountWriteHelpers one level only; and it reads SQL
// written in the function body (or naming its table by a constant in
// accountTableConstants), so a statement kept in a package-level constant or
// built elsewhere is not seen.
func TestAccountKeyedWritersTakeTheAccountLock(t *testing.T) {
	t.Parallel()
	guarded := map[string]bool{}
	for col, rule := range accountColumns {
		parts := strings.SplitN(col, ".", 3)
		if rule.kind == "deleted" && parts[0]+"."+parts[1] != "core.users" && !accountTablesWithForeignKey[parts[1]] {
			guarded[parts[1]] = true
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	seenExempt := map[string]bool{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == "migrations" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				key := filepath.ToSlash(rel) + ":" + fn.Name.Name
				body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
				writes, unknown := accountKeyedWrites(fn, body, guarded)
				for _, u := range unknown {
					offenders = append(offenders, key+": INSERT INTO a table named by "+u+": add it to accountTableConstants")
				}
				switch {
				case len(writes) == 0:
				case accountWriterExempt[key] != "":
					seenExempt[key] = true
				case !callsAny(fn.Body, accountLockCalls):
					offenders = append(offenders, key+" inserts into "+strings.Join(writes, ", "))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("inserts into tables account deletion clears, without the account-row lock (take lockAccountTx / withAccountRow, or list the function in accountWriterExempt with the reason):\n%s", strings.Join(offenders, "\n"))
	}
	for key := range accountWriterExempt {
		if !seenExempt[key] {
			t.Errorf("accountWriterExempt lists %s, which no longer inserts into such a table", key)
		}
	}
}

// accountInsertRe matches an INSERT's table, written out or named by a Go
// constant (`INSERT INTO `+chatSchedulesTable).
var accountInsertRe = regexp.MustCompile("(?i)insert\\s+into\\s+(?:(?:chat|core|sched)\\.)?(\\w+)|(?i)insert\\s+into\\s*`\\s*\\+\\s*(\\w+)")

// accountKeyedWrites lists the guarded tables fn's body inserts into, and the
// accountWriteHelpers it calls; unknown names the table constants it cannot
// read.
func accountKeyedWrites(fn *ast.FuncDecl, body string, guarded map[string]bool) (writes, unknown []string) {
	for _, m := range accountInsertRe.FindAllStringSubmatch(body, -1) {
		table := strings.ToLower(m[1])
		if m[2] != "" {
			if table = accountTableConstants[m[2]]; table == "" {
				unknown = append(unknown, m[2])
				continue
			}
		}
		if guarded[table] {
			writes = append(writes, table)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if name := calledName(n); accountWriteHelpers[name] {
			writes = append(writes, name+"()")
		}
		return true
	})
	return writes, unknown
}

// callsAny reports whether body calls a function named in names.
func callsAny(body *ast.BlockStmt, names map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		found = found || names[calledName(n)]
		return !found
	})
	return found
}

// calledName is the called function's name when n is a call, else "".
func calledName(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// Writes of an account's own rows made while the account is being deleted —
// by a request or turn still in flight, or by a server while the CLI deletes
// the account — are refused and leave no row: each waits on the account row
// DeleteUser holds, then finds no account. A participant's countdown in
// someone else's chat and a project memory it writes are refused the same
// way. Not parallel: it uses DeleteUser's seam.
func TestAccountWritesRacingDeletionLeaveNoRow(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const gone, mate = "racing-writer@x.com", "racing-mate@x.com"
	seedAccount(t, s, gone, mate)
	mateConv, err := s.CreateConversation(ctx, mate, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE chat.conversations SET multiplayer = TRUE WHERE id = $1`, mateConv.ID); err != nil {
		t.Fatal(err)
	}
	mateSrv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(mate))
	if err != nil {
		t.Fatal(err)
	}
	mateProject, err := s.CreateProject(ctx, &Project{OwnerEmail: mate, Name: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string]func() error{
		"Autonomous-mode acceptance": func() error { return s.AcceptAutonomousPolicy(ctx, gone, 0) },
		"push subscription": func() error {
			return s.UpsertPushSubscription(ctx, gone, "https://push.example/late", "a", "k")
		},
		"chat":   func() error { _, err := s.CreateConversation(ctx, gone, "t", "victoria", "", false); return err },
		"memory": func() error { _, err := s.CreateMemory(ctx, gone, "a late fact", "manual", ""); return err },
		"project memory": func() error {
			_, err := s.CreateProjectMemory(ctx, mateProject.ID, gone, "a late fact", "")
			return err
		},
		"skill": func() error { _, err := s.CreateUserSkill(ctx, gone, "late-skill", "d", "b"); return err },
		"persona": func() error {
			_, err := s.CreateUserPersona(ctx, gone, UserPersonaInput{Name: "late-persona", DisplayName: "Late", Instructions: "i"})
			return err
		},
		"connector preference": func() error {
			return s.SetConnectorPref(ctx, gone, ConnectorPref{Kind: ConnectorKindBundled, ConnectorID: "x", Enabled: true})
		},
		"sidebar layout": func() error { _, err := s.SetSidebarLayout(ctx, gone, []byte(`{}`), 0); return err },
		"project":        func() error { _, err := s.CreateProject(ctx, &Project{OwnerEmail: gone, Name: "late"}); return err },
		"connector": func() error {
			in := sampleServerInput(gone)
			_, err := s.CreateRemoteMCPServer(ctx, in)
			return err
		},
		"connector sign-in": func() error {
			return s.BeginOAuthFlow(ctx, "late-state", mateSrv.ID, gone, "verifier", time.Minute)
		},
		"countdown in a teammate's chat": func() error {
			_, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{ConversationID: mateConv.ID, UserEmail: gone, CallID: "c", ActionID: "a", ValuesJSON: "{}"})
			return err
		},
	}
	results := make(map[string]chan error, len(writes))
	for what := range writes {
		results[what] = make(chan error, 1)
	}
	deleteUserAfterGuard = func() {
		deleteUserAfterGuard = nil
		for what, write := range writes {
			go func() { results[what] <- write() }()
		}
		// Give the writes every chance to land before the deletion goes on.
		time.Sleep(300 * time.Millisecond)
	}
	t.Cleanup(func() { deleteUserAfterGuard = nil })
	if err := s.DeleteUser(ctx, gone); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for what, ch := range results {
		if err := <-ch; err == nil {
			t.Errorf("%s: written for the account while it was being deleted", what)
		}
	}
	for _, table := range []string{"chat.autonomous_acceptances", "chat.push_subscriptions", "chat.conversations", "chat.memories",
		"chat.user_skills", "chat.user_personas", "chat.user_connector_prefs", "chat.sidebar_layouts", "chat.remote_mcp_servers",
		"chat.remote_mcp_oauth_flow", "chat.ui_autosubmits"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE user_email = $1`, gone).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows outlived the account", table, n)
		}
	}
	var projects int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.projects WHERE owner_email = $1`, gone).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 0 {
		t.Errorf("%d projects outlived the account", projects)
	}
	// After the deletion the same writes are refused at once.
	for what, write := range writes {
		if err := write(); err == nil {
			t.Errorf("%s: written for an account that no longer exists", what)
		}
	}
}

// A writer that turns a participant's approval into a queued message while
// holding only the chat row (an approval resume, a countdown firing) and an
// account deletion: the deletion locks the chat first, so the message is
// removed with the rest of the account's rows instead of outliving it.
func TestChatFirstWriterRacingDeletionLeavesNoQueuedMessage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const gone, mate = "resumed-participant@x.com", "resume-owner@x.com"
	seedAccount(t, s, gone, mate)
	conv, err := s.CreateConversation(ctx, mate, "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE chat.conversations SET multiplayer = TRUE WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat.approvals (id, conversation_id, user_email, tool_name, args_json, status, resume_state, created_at) VALUES ('armed-card', $1, $2, 't', '{}', 'approved', 'armed', 1)`, conv.ID, gone); err != nil {
		t.Fatal(err)
	}
	// The writer, as ClaimApprovalResume runs: the chat row, then the message.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM chat.conversations WHERE id = $1 FOR UPDATE`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.insertInputTx(ctx, tx, InputQueueRow{ID: "resume-input", ConversationID: conv.ID, UserEmail: gone, ClientInputID: "approval-resume:armed-card", Message: "m", Attachments: "[]", Mode: InputModeResume, State: InputStateQueued}); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteUser(ctx, gone) }()
	select {
	case err := <-deleted:
		t.Fatalf("the deletion finished (err %v) while a writer held a chat the account has an approval in", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.chat_input_queue WHERE user_email = $1`, gone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d queued messages outlived the participant's account", n)
	}
}

// DeleteAccount deletes only the account the caller read: when the email
// names another account by then (deleted and created again in between), it
// deletes nothing.
func TestDeleteAccountRefusesAnotherAccountUnderTheEmail(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "same@x.com")
	id, err := s.AccountForDeletion(ctx, "Same@X.com")
	if err != nil || id == "" {
		t.Fatalf("AccountForDeletion = %q, %v", id, err)
	}
	if _, err := s.DeleteAccount(ctx, "same@x.com", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrAccountChanged) {
		t.Fatalf("another account's id: err %v, want ErrAccountChanged", err)
	}
	if _, err := s.GetUser(ctx, "same@x.com"); err != nil {
		t.Fatalf("the account was deleted: %v", err)
	}
	got, err := s.DeleteAccount(ctx, "same@x.com", id)
	if err != nil || got != id {
		t.Fatalf("DeleteAccount = %q, %v; want %q", got, err, id)
	}
	if _, err := s.AccountForDeletion(ctx, "same@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("after the deletion: err %v, want ErrUserNotFound", err)
	}
}

// AccountEmailByID reads the address of an account by its id, and answers
// ErrUserNotFound once the account is deleted (or for an id no account has).
func TestAccountEmailByID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "byid@x.com")
	id, err := s.AccountForDeletion(ctx, "byid@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if email, err := s.AccountEmailByID(ctx, id); err != nil || email != "byid@x.com" {
		t.Fatalf("AccountEmailByID = %q, %v; want byid@x.com", email, err)
	}
	if _, err := s.AccountEmailByID(ctx, "not-an-id"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("an id no account has: err %v, want ErrUserNotFound", err)
	}
	if _, err := s.DeleteAccount(ctx, "byid@x.com", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountEmailByID(ctx, id); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("after the deletion: err %v, want ErrUserNotFound", err)
	}
}

// A running server holds the presence lock shared, and an account deletion
// from the CLI cannot take it exclusive meanwhile; once the server lets go
// it can. A server starting while the CLI holds it waits for it — without a
// deadline, until it is released — and a server told to stop while it waits
// gives up holding nothing.
func TestServerPresenceExcludesCLIDeletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	release, err := s.HoldServerPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExcludeServers(ctx); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("with a server running: err %v, want ErrServerRunning", err)
	}
	release()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM core.server_presence`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("heartbeat rows after the server stopped = %d, %v; want none", n, err)
	}
	unlock, err := s.ExcludeServers(ctx)
	if err != nil {
		t.Fatalf("with no server: %v", err)
	}
	type held struct {
		release func()
		err     error
	}
	started := make(chan held, 1)
	go func() {
		r, err := s.HoldServerPresence(ctx)
		started <- held{r, err}
	}()
	select {
	case h := <-started:
		if h.release != nil {
			h.release()
		}
		t.Fatalf("a server started (err %v) while a CLI deletion held the lock", h.err)
	case <-time.After(1500 * time.Millisecond):
	}
	// Told to stop while it waits: it gives up.
	stopCtx, stop := context.WithCancel(ctx)
	gaveUp := make(chan error, 1)
	go func() {
		r, err := s.HoldServerPresence(stopCtx)
		if r != nil {
			r()
		}
		gaveUp <- err
	}()
	stop()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a server stopped while waiting: err %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a server told to stop kept waiting")
	}
	unlock()
	select {
	case h := <-started:
		if h.err != nil {
			t.Fatal(h.err)
		}
		h.release()
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not start after the CLI deletion ended")
	}
}

// WithAccountRowByID runs its function with the account's address while a
// deletion of the account waits for it; an id no account has runs nothing.
func TestWithAccountRowByID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "row@x.com")
	id, err := s.AccountForDeletion(ctx, "row@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if found, err := s.WithAccountRowByID(ctx, "not-an-id", func(string) error {
		t.Error("ran for an id no account has")
		return nil
	}); found || err != nil {
		t.Fatalf("an id no account has: found %v, err %v", found, err)
	}
	failed := errors.New("restore failed")
	if found, err := s.WithAccountRowByID(ctx, id, func(string) error { return failed }); !found || !errors.Is(err, failed) {
		t.Fatalf("a failing function: found %v, err %v; want found and its error", found, err)
	}
	inside, proceed := make(chan string), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		_, err := s.WithAccountRowByID(ctx, id, func(email string) error {
			inside <- email
			<-proceed
			return nil
		})
		held <- err
	}()
	if email := <-inside; email != "row@x.com" {
		t.Fatalf("ran with %q, want row@x.com", email)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := s.DeleteAccount(ctx, "row@x.com", id)
		deleted <- err
	}()
	select {
	case err := <-deleted:
		close(proceed)
		t.Fatalf("the deletion finished (err %v) while the row was held", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(proceed)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("the deletion after the row was let go: %v", err)
	}
}

// takePresenceLockFromTheServer drops the connection a running server holds
// its presence lock on (as a database restart does) and takes the lock
// exclusive the moment it is free, as a CLI deletion would, on a connection
// of its own; it returns the function that lets go. A server quick enough to
// take its lock back first gets its connection dropped again.
func takePresenceLockFromTheServer(t *testing.T, s *Store) (letGo func()) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := s.db.ExecContext(ctx, `
			SELECT pg_terminate_backend(pid) FROM pg_locks
			 WHERE locktype = 'advisory' AND granted AND objsubid = 1
			   AND classid::bigint = $1 AND objid::bigint = $2
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			serverPresenceLockKey>>32, serverPresenceLockKey&0xffffffff); err != nil {
			t.Fatal(err)
		}
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var got bool
			if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, serverPresenceLockKey).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got {
				return func() {
					_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, serverPresenceLockKey)
					_ = conn.Close()
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = conn.Close()
	}
	t.Fatal("never took the presence lock from the server")
	return nil
}

// waitPresenceLost waits until s.ServerPresenceLost reports want.
func waitPresenceLost(t *testing.T, s *Store, want bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for s.ServerPresenceLost() != want {
		if time.Now().After(deadline) {
			t.Fatalf("ServerPresenceLost stayed %v", !want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A server whose presence lock connection drops reports its presence lost
// until it holds the lock again; while a CLI deletion holds the lock
// exclusive it stays lost (writing its heartbeat, so the CLI's second look
// sees it), and once the CLI lets go it takes the lock back and reports it
// held.
func TestServerPresenceLostUntilTheLockIsTakenAgain(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	release, err := s.HoldServerPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if s.ServerPresenceLost() {
		t.Fatal("a server holding its lock reports it lost")
	}
	// A long outage: the row is stale, so only a fresh write counts.
	if _, err := s.db.ExecContext(ctx, `UPDATE core.server_presence SET last_seen = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	letGo := takePresenceLockFromTheServer(t, s)
	waitPresenceLost(t, s, true)
	time.Sleep(1500 * time.Millisecond)
	if !s.ServerPresenceLost() {
		t.Fatal("the server reported its presence held while a CLI deletion held the lock")
	}
	var fresh bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core.server_presence WHERE last_seen > now() - interval '10 seconds')`).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("no fresh heartbeat from the server taking its lock back (%v, %v)", fresh, err)
	}
	letGo()
	waitPresenceLost(t, s, false)
	var got bool
	if err := s.db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, serverPresenceLockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		_, _ = s.db.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, serverPresenceLockKey)
		t.Fatal("the server reports its presence held without holding the lock")
	}
}

// A CLI deletion looks at the heartbeats again after serverPresenceGrace,
// holding the lock: a row written meanwhile — a server that lost its lock
// connection and is taking it back — refuses it, although every row was
// stale when it took the lock.
func TestCLIDeletionLooksAgainBeforeDeleting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO core.server_presence (instance_id, last_seen) VALUES ('reconnecting', now() - interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		unlock, err := s.ExcludeServers(ctx)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	time.Sleep(time.Second)
	if _, err := s.db.ExecContext(ctx, `UPDATE core.server_presence SET last_seen = now()`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrServerRunning) {
		t.Fatalf("err %v, want ErrServerRunning from the second look", err)
	}
}

// A server whose lock connection dropped still has its heartbeat: a CLI
// deletion refuses while a row is fresh, even with the lock free, and goes
// ahead once the row is stale (a crashed server's).
func TestCLIDeletionRefusedOnAFreshHeartbeat(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO core.server_presence (instance_id, last_seen) VALUES ('reconnecting', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExcludeServers(ctx); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("with a fresh heartbeat: err %v, want ErrServerRunning", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE core.server_presence SET last_seen = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.ExcludeServers(ctx)
	if err != nil {
		t.Fatalf("with a stale heartbeat: %v", err)
	}
	unlock()
}
