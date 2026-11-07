// account — the signed-in account's own record, GET /api/me (#2000).
//
// Chat, the projects modal, the patrol launchpad and three Settings pages
// each read the caller's role / team / admin flag on mount, several of them
// from /api/me/team, which also computes the Leave-team impact counts (two
// extra queries) that only Settings → Team shows. /api/me answers the same
// four fields in one query (internal/httpapi/me.go meResponse), so they all
// read it through fetchMe: concurrent callers share one request, and an
// answer is reused for ME_TTL_MS so a page that mounts several readers asks
// once. Settings → Team keeps /me/team for the impact counts, and calls
// invalidateMe() after it changes the team.
//
// A failed read (network, non-2xx, unreadable body) answers null and is not
// cached, so the next caller retries. Every caller treats the record as a
// presentation hint: the server stays the authority on what a role may do.

export type Me = {
  email: string;
  role: string;
  team_id: string;
  /** Effective admin flag (ADMIN_EMAILS or role admin). */
  admin: boolean;
};

export const ME_TTL_MS = 10_000;

let cached: Me | null = null;
let cachedAt = 0;
let inflight: Promise<Me | null> | null = null;
// Bumped by invalidateMe so a read already in flight when the team changed
// does not repopulate the cache with the old record.
let generation = 0;

/**
 * The caller's account record, or null when it could not be read.
 * `fresh` skips the cache (not an in-flight read started after the last
 * invalidation).
 */
export function fetchMe(opts: { fresh?: boolean } = {}): Promise<Me | null> {
  if (!opts.fresh && cached && Date.now() - cachedAt < ME_TTL_MS) {
    return Promise.resolve(cached);
  }
  if (inflight) return inflight;
  const p: Promise<Me | null> = readMe(generation).finally(() => {
    if (inflight === p) inflight = null;
  });
  inflight = p;
  return p;
}

async function readMe(gen: number): Promise<Me | null> {
  try {
    const res = await fetch("/api/me", { cache: "no-store" });
    if (!res.ok) return null;
    const me = (await res.json()) as Partial<Me> | null;
    if (!me || typeof me !== "object") return null;
    const record: Me = {
      email: typeof me.email === "string" ? me.email : "",
      role: typeof me.role === "string" ? me.role : "",
      team_id: typeof me.team_id === "string" ? me.team_id : "",
      admin: me.admin === true,
    };
    if (gen === generation) {
      cached = record;
      cachedAt = Date.now();
    }
    return record;
  } catch {
    return null;
  }
}

/** Drops the cached record (and detaches an in-flight read from the cache). */
export function invalidateMe(): void {
  cached = null;
  cachedAt = 0;
  inflight = null;
  generation++;
}

/** Test seam: forget everything between cases. */
export function _resetMeForTests(): void {
  invalidateMe();
}
