import { formatCompactCronCadence } from "@/app/shared/lib/patrolSchedule";

/**
 * cadenceWithZone names a patrol's rhythm, with its time zone when the
 * schedule fires at a clock time ("Weekdays 08:00 · America/New_York") —
 * an interval ("Every 2h") reads the same in every zone.
 */
export function cadenceWithZone(cron: string, timezone: string): string {
  const cadence = formatCompactCronCadence(cron);
  // An interval reads the same in every zone only when nothing else in it is
  // tied to the clock: "*/15 * * * *" or "0 */2 * * *". A restricted field
  // ("*/15 9-17 * * *", "0 */2 * * 1-5") is read in the patrol's zone, so the
  // zone is shown.
  const f = cron.trim().split(/\s+/);
  const allWild = (xs: string[]) => xs.every((x) => x === "*");
  const interval =
    f.length === 5 &&
    ((/^(\*|\*\/\d+)$/.test(f[0]) && allWild(f.slice(1))) ||
      (/^(\*|0)$/.test(f[0]) && /^(\*|\*\/\d+)$/.test(f[1]) && allWild(f.slice(2))));
  const tz = timezone.trim();
  return tz && !interval ? `${cadence} · ${tz}` : cadence;
}
