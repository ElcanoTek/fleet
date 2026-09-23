// Timezone helpers for the task form's Repeat schedule. A recurring task's cron
// expression fires at the wall-clock time in the task's own IANA zone
// (models.TaskCreate.Timezone); a create request that names none falls back to
// the server's FLEET_DEFAULT_TIMEZONE, then UTC. The form therefore always
// sends an explicit zone — the browser's by default — so "8:00 AM" means 8:00
// AM where the author is, and names that zone rather than saying "local time".

// browserTimeZone is the viewer's IANA zone (e.g. "America/New_York"), or
// "UTC" when the runtime cannot resolve one.
export function browserTimeZone(): string {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (zone) return zone;
  } catch {
    // Fall through: an Intl without zone support behaves like UTC.
  }
  return "UTC";
}

// isValidTimeZone reports whether the runtime recognizes an IANA zone name.
export function isValidTimeZone(zone: string): boolean {
  if (!zone.trim()) return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

// canonicalTimeZone resolves an alias to the runtime's canonical name
// ("US/Eastern" → "America/New_York"), or returns the input unchanged when the
// runtime doesn't know it.
export function canonicalTimeZone(zone: string): string {
  try {
    return new Intl.DateTimeFormat("en-US", { timeZone: zone }).resolvedOptions().timeZone || zone;
  } catch {
    return zone;
  }
}

// sameTimeZone reports whether two zone names are the same zone, aliases
// included — the backend preserves whatever IANA name a task was saved with.
export function sameTimeZone(a: string, b: string): boolean {
  return a === b || canonicalTimeZone(a) === canonicalTimeZone(b);
}

// timeZoneOptions lists the zones the picker offers: every zone the runtime
// knows, with UTC and any `include` entries (the browser's zone, an edited
// task's stored zone) guaranteed present even on a runtime that can't
// enumerate — so the current value is always selectable.
export function timeZoneOptions(...include: string[]): string[] {
  let zones: string[] = [];
  try {
    const supported = (Intl as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf;
    if (supported) zones = supported("timeZone");
  } catch {
    zones = [];
  }
  const all = new Set<string>(zones);
  for (const zone of ["UTC", ...include]) {
    if (zone && zone.trim()) all.add(zone);
  }
  return [...all].sort((a, b) => {
    // UTC first, then alphabetical: it is the one zone every operator reaches for.
    if (a === "UTC") return -1;
    if (b === "UTC") return 1;
    return a.localeCompare(b);
  });
}

// timeZoneAbbreviation is the short name the zone uses at `at` ("EDT", "GMT+2"),
// or "" when the runtime can't format it.
export function timeZoneAbbreviation(zone: string, at: Date = new Date()): string {
  try {
    const part = new Intl.DateTimeFormat("en-US", { timeZone: zone, timeZoneName: "short" })
      .formatToParts(at)
      .find((p) => p.type === "timeZoneName");
    return part?.value ?? "";
  } catch {
    return "";
  }
}

// formatTimeZoneLabel renders a zone for the schedule echo: the IANA name plus
// its current abbreviation when that adds information ("America/New_York (EDT)",
// but plain "UTC").
export function formatTimeZoneLabel(zone: string, at: Date = new Date()): string {
  const abbr = timeZoneAbbreviation(zone, at);
  if (!abbr || abbr === zone) return zone;
  return `${zone} (${abbr})`;
}
