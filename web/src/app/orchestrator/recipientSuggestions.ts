import type { RecipientUse } from "@/app/shared/lib/orchestratorApi";

// The task form's **Email results** field remembers who the team mails: the
// server returns every address visible tasks already deliver to
// (GET /tasks/recipients), and this picks which few to offer for what has been
// typed so far. Pure, so the ranking is unit-tested apart from the combobox.

/** How many suggestions the field shows at once. */
export const MAX_RECIPIENT_SUGGESTIONS = 8;

// Where a typed fragment counts as the start of a word in an address:
// "blu" should find jane.blumin@weather.com, and "weather" every weather.com
// address, before a match buried mid-word does.
const WORD_START = /[@._+-]/;

function matchRank(email: string, query: string): number {
  if (email.startsWith(query)) return 0;
  const at = email.indexOf(query);
  if (at < 0) return -1;
  return WORD_START.test(email[at - 1] ?? "") ? 1 : 2;
}

/**
 * rankRecipientSuggestions returns the addresses to offer: none already in the
 * field; for an empty query the most used (then most recent); for a typed
 * fragment, matches only, word-start matches ahead of mid-word ones, then the
 * same usage order.
 */
export function rankRecipientSuggestions(
  history: RecipientUse[],
  query: string,
  selected: string[],
  limit = MAX_RECIPIENT_SUGGESTIONS,
): RecipientUse[] {
  const q = query.trim().toLowerCase();
  const taken = new Set(selected.map((s) => s.toLowerCase()));
  const byUse = (a: RecipientUse, b: RecipientUse) =>
    b.task_count - a.task_count || b.last_used.localeCompare(a.last_used) || a.email.localeCompare(b.email);
  const pool = history.filter((r) => !taken.has(r.email.toLowerCase()));
  if (!q) return [...pool].sort(byUse).slice(0, limit);
  return pool
    .map((r) => ({ r, rank: matchRank(r.email.toLowerCase(), q) }))
    .filter((x) => x.rank >= 0)
    .sort((a, b) => a.rank - b.rank || byUse(a.r, b.r))
    .slice(0, limit)
    .map((x) => x.r);
}
