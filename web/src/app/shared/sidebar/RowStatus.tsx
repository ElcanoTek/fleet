import { t as tr } from "@/app/lib/i18n";
import type { ReactNode } from "react";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Icon } from "@/app/shared/ui/Icon";

// RowStatus — the one quiet status at a sidebar row's right edge
// (docs/SIDEBAR.md). A row is its title; it says one more thing only when
// that thing matters, and the first of these that applies is the one shown:
//
//   1. working      a reply is being written right now
//   2. days left    a temporary chat inside the warning window
//   3. patrol       when it checks next, or that it is paused
//   4. live         a Multiplayer chat teammates can write in
//   5. shared       with the team, then by link
//   6. sealed       a private, locked-down chat
//
// A teammate's chat also carries its owner's initial, after the status:
// whose it is does not compete with what state it is in. Everything else a
// chat can be (labelled, temporary but not close to its end) stays off the
// row and in its menu, its tooltip and search.
//
// The status is decoration for sighted readers; the sentence it stands for
// goes to a screen reader through the row's description (`describe`).

export type RowPatrol = {
  status: string;
  /** Unix seconds; absent when not known (a patrol made a moment ago). */
  nextRunAt?: number;
};

export type RowStatusInput = {
  streaming?: boolean;
  /** Whole days until a temporary chat is deleted, inside the warning window. */
  expiresIn?: number | null;
  patrol?: RowPatrol | null;
  live?: boolean;
  teamShared?: boolean;
  linkShared?: boolean;
  sealed?: boolean;
  /** A teammate's chat: the owner's email. */
  owner?: string;
};

/** "19h", "45m", "2d": how long until a unix-seconds moment, to one unit. */
export function compactUntil(at: number, now: number): string {
  const s = Math.max(0, at - now);
  if (s < 60) return tr("now");
  if (s < 3600) return tr("{0}m", [Math.max(1, Math.round(s / 60))]);
  if (s < 86400) return tr("{0}h", [Math.max(1, Math.round(s / 3600))]);
  return tr("{0}d", [Math.max(1, Math.round(s / 86400))]);
}

type Shown = { key: string; node: ReactNode; text: string };

function primaryStatus(s: RowStatusInput, now: number): Shown | null {
  if (s.streaming) {
    return {
      key: "working",
      text: tr("Working…"),
      node: <span className="inline-block size-1.5 animate-pulse rounded-full bg-[var(--color-accent)]" />,
    };
  }
  if (typeof s.expiresIn === "number") {
    const days = Math.max(1, s.expiresIn);
    return {
      key: "expiring",
      text:
        s.expiresIn <= 1
          ? tr("Temporary — will be deleted within a day. Send a message in it, pin it, move it into a project or archive it to keep it.")
          : tr("Temporary — will be deleted in {0} days. Send a message in it, pin it, move it into a project or archive it to keep it.", [s.expiresIn]),
      node: (
        <span className="inline-flex items-center gap-0.5 text-[0.72rem] font-medium tabular-nums text-[var(--color-warning-text)]">
          <Icon name="hourglass" className="size-3 shrink-0" />
          {tr("{0}d", [days])}
        </span>
      ),
    };
  }
  if (s.patrol) {
    const paused = s.patrol.status === "paused";
    const next = !paused && s.patrol.nextRunAt ? compactUntil(s.patrol.nextRunAt, now) : "";
    return {
      key: paused ? "patrol-paused" : "patrol",
      text: paused
        ? tr("Patrol — paused")
        : next
          ? tr("Patrol — next check in {0}", [next])
          : tr("Patrol — this chat checks in on a schedule"),
      node: (
        <span
          className={[
            "inline-flex items-center gap-1 text-[0.72rem] font-medium tabular-nums",
            paused ? "text-[var(--color-warning-text)]" : "text-[var(--color-link)]",
          ].join(" ")}
        >
          <Icon name="activity" className="size-3 shrink-0" />
          {paused ? tr("Paused") : next}
        </span>
      ),
    };
  }
  if (s.live) {
    return {
      key: "live",
      text: tr("Multiplayer — teammates can chat here"),
      node: <span className="inline-block size-2 rounded-full bg-[var(--color-state-ok)]" />,
    };
  }
  if (s.teamShared) {
    return {
      key: "team",
      text: tr("Shared with your team — teammates can read it"),
      node: <Icon name="users" className="size-3.5 shrink-0 text-[var(--color-link)]" />,
    };
  }
  if (s.linkShared) {
    return {
      key: "link",
      text: tr("Shared by link — anyone with the link"),
      node: <Icon name="link" className="size-3.5 shrink-0 text-[var(--color-link)]" />,
    };
  }
  if (s.sealed) {
    return {
      key: "sealed",
      text: tr("Sealed — private chat"),
      node: <Icon name="lock" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />,
    };
  }
  return null;
}

/** The sentence(s) a row's status stands for, for its tooltip and screen readers. */
export function describeRowStatus(s: RowStatusInput, now: number): string {
  const parts: string[] = [];
  const shown = primaryStatus(s, now);
  if (shown) parts.push(shown.text);
  if (s.owner) parts.push(tr("Shared by {0}", [s.owner]));
  return parts.join(" · ");
}

/** OwnerAvatar: a teammate's initial on a colour that is always theirs. */
export function OwnerAvatar({ email, className = "" }: { email: string; className?: string }) {
  return (
    <span
      aria-hidden="true"
      data-testid="row-owner"
      className={`rail-avatar ${className}`}
      style={labelChipStyle(email.trim().toLowerCase())}
    >
      {(email.trim().charAt(0) || "?").toUpperCase()}
    </span>
  );
}

export function RowStatus({ status, now }: { status: RowStatusInput; now: number }) {
  const shown = primaryStatus(status, now);
  if (!shown && !status.owner) return null;
  return (
    <span
      aria-hidden="true"
      data-testid="row-status"
      data-status={shown?.key}
      title={describeRowStatus(status, now)}
      className="ml-auto inline-flex shrink-0 items-center gap-1.5 pl-1"
    >
      {shown?.node}
      {status.owner ? <OwnerAvatar email={status.owner} /> : null}
    </span>
  );
}
