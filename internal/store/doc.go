// Package store is the Postgres-backed conversation persistence layer.
//
// A single Postgres database holds all users' conversations and the
// event-level message log produced by [agent.RunTurn]. Every read path is
// scoped by user_email — there is no superuser query — so cross-user access
// requires an auth-layer breach first.
//
// # Retention
//
// Unpinned conversations expire after CONVERSATION_TTL_DAYS (default 14) of
// inactivity. Pinned conversations are exempt from TTL and cap eviction
// and are kept indefinitely. The [Store.SweepExpired] routine runs at
// server startup and after every successful turn.
//
// # Schema
//
// See migrations/001_initial.sql (embedded). Core tables:
//
//   - conversations: one row per chat thread, with persona, pinned flag,
//     and updated_at timestamp.
//   - messages: one row per streaming event (user text, assistant text,
//     reasoning block, tool call, tool result), FK-cascaded on conversation
//     delete.
//   - approvals: pending high-risk tool calls awaiting user consent.
//   - turn_metrics: per-turn cost + token counters for the admin dashboard.
//   - users: provisioned accounts with bcrypt password hashes.
//
// Message content is stored as a JSON blob whose shape lives in
// [agent.HistoryEntry]; this keeps the schema flexible without losing the
// ability to index on (conversation_id, id) for ordered reads.
package store
