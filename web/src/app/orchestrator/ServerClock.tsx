"use client";

import { useCallback, useEffect, useState } from "react";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { Icon } from "@/app/shared/ui/Icon";

// ServerClock — the running server-side wall clock, shown beside the dashboard's
// agent-status cards.
//
// It answers a question the Operations Center could not answer before: a cron
// recurrence fires in the SERVER's zone, not the operator's, so "0 9 * * *" runs
// at 9am somewhere the UI never named. Reading the clock off the operator's own
// machine would beg that question — it would just restate local time — so the
// time comes from GET /api/config, which reports the orchestrator's own now.
//
// The skew, not the timestamp, is what we keep: one fetch establishes how far
// this browser's clock sits from the server's, and the display then ticks
// locally against that offset. That keeps it honest when a laptop clock is
// wrong (the usual reason a "server time" readout would lie) without polling
// once a second, and it degrades gracefully — a browser whose ICU data has
// never heard of the configured zone still renders the right wall time, because
// the offset travelled with the timestamp.

// Refetch hourly so a long-lived dashboard survives a DST transition and any
// slow drift between the two clocks. The tick itself is local.
const RESYNC_MS = 60 * 60 * 1000;

// formatServerTime renders `instant` in `timeZone`. A zone the runtime rejects
// (an obscure or misspelled name) falls back to the offset the server's own
// timestamp carried, which is why the caller keeps it.
function formatServerTime(instant: Date, timeZone: string | undefined, fallbackOffsetMin: number) {
  if (timeZone) {
    try {
      return new Intl.DateTimeFormat(undefined, {
        hour: "numeric",
        minute: "2-digit",
        second: "2-digit",
        timeZone,
        timeZoneName: "short",
      }).format(instant);
    } catch {
      /* unknown zone — fall through to the fixed-offset path */
    }
  }
  // Fixed-offset rendering: shift the instant and format as UTC, so the digits
  // are the server's wall clock even with no zone database entry to consult.
  const shifted = new Date(instant.getTime() + fallbackOffsetMin * 60_000);
  const hhmmss = new Intl.DateTimeFormat(undefined, {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
    timeZone: "UTC",
  }).format(shifted);
  const sign = fallbackOffsetMin < 0 ? "-" : "+";
  const abs = Math.abs(fallbackOffsetMin);
  const offsetLabel = `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
  return `${hhmmss} ${offsetLabel}`;
}

export function ServerClock() {
  // One fetch on mount and one per resync tick — the hook re-runs when its dep
  // changes, so the counter is the whole resync mechanism.
  const [resyncNonce, setResyncNonce] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setResyncNonce((n) => n + 1), RESYNC_MS);
    return () => clearInterval(id);
  }, []);

  const { data: config } = useCancellableFetch(
    useCallback(() => orchestratorApi.config(), []),
    [resyncNonce],
  );
  const serverTime = config?.server_time;

  // now starts null so the first render (server-side and pre-fetch) emits
  // nothing: a clock rendered before the skew is known would be local time
  // wearing a server-time label, and it would also hydrate-mismatch.
  const [now, setNow] = useState<Date | null>(null);

  useEffect(() => {
    if (!serverTime) return;
    const parsed = Date.parse(serverTime);
    if (Number.isNaN(parsed)) return;
    // Measured once against the response we just received; every tick after
    // this is local arithmetic.
    const skewMs = parsed - Date.now();
    const tick = () => setNow(new Date(Date.now() + skewMs));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [serverTime]);

  if (!now || !serverTime) return null;

  // The offset the server sent, for the no-such-zone fallback. Date.parse drops
  // it, so read it off the string.
  const match = /([+-])(\d{2}):(\d{2})$/.exec(serverTime);
  const fallbackOffsetMin = match
    ? (match[1] === "-" ? -1 : 1) * (Number(match[2]) * 60 + Number(match[3]))
    : 0;

  const zone = config?.timezone?.trim() || undefined;
  const taskZone = config?.default_task_timezone?.trim() || undefined;
  // Name the scheduling zone too when it differs from the server clock — that is
  // the one a cron actually fires in, and a mismatch is exactly the trap this
  // readout exists to expose.
  const title = [
    zone ? `Server timezone: ${zone}` : "Server time",
    taskZone && taskZone !== zone ? `New tasks default to: ${taskZone}` : null,
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="server-clock" data-testid="server-clock" title={title}>
      <Icon name="clock" className="server-clock-icon" />
      <span className="server-clock-label">Server time</span>
      {/* aria-live is deliberately off: a clock announcing itself every second
          would make a screen reader unusable. The dateTime attribute carries the
          machine-readable instant for anyone who asks for it. */}
      <time className="server-clock-time" dateTime={now.toISOString()}>
        {formatServerTime(now, zone, fallbackOffsetMin)}
      </time>
    </div>
  );
}

export default ServerClock;
