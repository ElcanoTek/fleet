// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.
//
// The legacy two-database layout (ADR-0005, superseded by ADR-0098): chat and
// sched in two databases, each in its own public schema. Every existing
// install has it until `fleet db consolidate` moves it into one database;
// this file is the only Go code that resolves it, so that removing it once
// every box is consolidated is mechanical (docs/ONE-DATABASE.md, "Removing
// the legacy layout").

package config

// LayoutLegacy is the pre-#2049 layout: two databases, each store in its own
// database's public schema.
const LayoutLegacy Layout = "legacy" // LEGACY-DB-LAYOUT: remove after consolidation

// resolveLegacyDSNs resolves the configuration the legacy layout used, with
// its original precedence:
//
//	chat:  FLEET_CHAT_DATABASE_URL, else DATABASE_URL, else the DB_* parts
//	sched: FLEET_SCHED_DATABASE_URL, else SCHED_DATABASE_URL, else
//	       DATABASE_URL, else the DB_* parts
//
// with the per-store DB_* defaults user/database chat/chat and fleet/sched.
// It answers (ok) only for the shapes the one-DSN configuration cannot
// express — a store-specific key is set, or neither DATABASE_URL nor DB_NAME
// is, so the per-store defaults decide — and returns LayoutLegacy when the
// two DSNs name two databases. When they name one database (both keys set to
// it, say) it returns that database in the one-database layout instead.
//
// The bare per-store defaults stay here deliberately: a box that relied on
// them (a dev box with chat and sched databases and no DSN configured) must
// not be pointed at a different, empty database by an upgrade. They become
// the single fleet/fleet default when this file is deleted.
func resolveLegacyDSNs(get func(string) string) (DSNs, bool) {
	perStore := get("FLEET_CHAT_DATABASE_URL") != "" || get("FLEET_SCHED_DATABASE_URL") != "" || get("SCHED_DATABASE_URL") != ""
	if !perStore && (get("DATABASE_URL") != "" || get("DB_NAME") != "") {
		return DSNs{}, false
	}
	var d DSNs
	d.Chat, d.ChatFrom = firstDSN(get, "FLEET_CHAT_DATABASE_URL", "DATABASE_URL")
	if d.Chat == "" {
		d.Chat, d.ChatFrom = dbPartsDSN(get, "chat", "chat")
	}
	d.Sched, d.SchedFrom = firstDSN(get, "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL", "DATABASE_URL")
	if d.Sched == "" {
		d.Sched, d.SchedFrom = dbPartsDSN(get, "fleet", "sched")
	}
	if sameDatabase(d.Chat, d.Sched) {
		return schemaDSNs(d.Chat, d.Sched, d.ChatFrom, d.SchedFrom), true
	}
	d.Layout, d.ChatSchema, d.SchedSchema = LayoutLegacy, PublicSchema, PublicSchema
	return d, true
}

// LayoutOf is the layout of an already-resolved DSN pair (cfg.DatabaseURL /
// cfg.SchedDatabaseURL): LayoutSchemas when they name the same database,
// else LayoutLegacy. Callers use it only to report the legacy layout.
func LayoutOf(chatDSN, schedDSN string) Layout { // LEGACY-DB-LAYOUT: remove after consolidation
	if sameDatabase(chatDSN, schedDSN) {
		return LayoutSchemas
	}
	return LayoutLegacy
}
