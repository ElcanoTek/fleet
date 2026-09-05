"use client";

// Settings → Team (#1157) — the member-facing half of team membership.
//
// Teams were writable only from Settings → Admin → Users, so on a box whose
// only admin came from the ADMIN_EMAILS env allowlist nobody could get into a
// team, and every "Share with my team" project failed with "you are not in a
// team". This page states the model plainly and lets a user act on it:
//
//   - no team  → name one and create it (self-serve),
//   - in a team → see it, and leave it,
//   - a name someone else already uses → 409 upstream, with the "ask an admin"
//     path spelled out. Joining an existing trust group stays admin-granted
//     because a shared team_id is what exposes team-shared projects and
//     team-visible conversations (ADR-0013 / ADR-0047).
//
// Admins are exempt upstream (they can type any team here, same as the Users
// tab); the copy says so rather than pretending the gate applies to them.
//
// The copy tells the truth about the model in both directions. Creating a team
// never invites teammates to "join the same name" — the server refuses a name
// somebody else holds (409), so an admin adds them from the Users tab. And
// LEAVING states its consequences before it happens (the counts come from GET
// /api/me/team): the team-shared projects that go out of view, and the chats
// this user shared into them, which are unshared by leaving because a
// team-shared chat cannot outlive the place teammates would find it
// (ADR-0057).

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";

import { btnClass, SETTINGS_INPUT } from "../ui/atoms";
import { ConnPanel, ConnPanelHead, ConnPanelSub, SetSection } from "../ui/panels";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { DialogShell } from "@/app/shared/ui/DialogShell";

export type Me = {
  email: string;
  role: string;
  team_id: string;
  admin: boolean;
  // What leaving would cost, computed server-side (GET /api/me/team):
  // shared_projects = team-shared projects owned by OTHERS that go out of
  // view; shared_chats = this user's own chats currently shared with the team,
  // all of which leaving unshares. ABSENT when the server could not count
  // them (the fields are omitempty pointers on purpose) → the confirm says it
  // doesn't know rather than reporting a zero it never computed. A dialog
  // whose whole job is stating consequences must not degrade to "nothing to
  // lose".
  shared_projects?: number;
  shared_chats?: number;
};

export default function TeamSettingsPage() {
  const [me, setMe] = useState<Me | null>(null);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  // Leaving is destructive in a way the old fire-then-report flow never said:
  // it unshares chats. true = the confirm is open.
  const [confirmLeave, setConfirmLeave] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await fetch("/api/me/team", { cache: "no-store" });
      if (res.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Request failed (${res.status}).`));
      setMe((await res.json()) as Me);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to read your team.");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    // queueMicrotask keeps the first setState out of the effect body itself
    // (react-hooks/set-state-in-effect), the same shape the projects modal uses.
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void load();
    });
    return () => {
      cancelled = true;
    };
  }, [load]);

  const write = async (teamID: string) => {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const res = await fetch("/api/me/team", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ team_id: teamID }),
      });
      if (res.status === 401) {
        // The session lapsed between the read and the write: the load path
        // redirects, so this one does too rather than rendering "Save failed
        // (401)" over a form that can no longer save anything.
        window.location.href = "/login";
        return;
      }
      if (res.status === 409) {
        // The server refuses a name that is already in use — by another
        // member OR by a team-shared project whose team has no members left
        // (ADR-0047). It cannot say which, so neither does this: the fix is
        // the same either way.
        throw new Error(
          "That name is already in use. An admin can add you to the team in Settings → Admin → Users.",
        );
      }
      if (!res.ok) throw new Error(await readError(res, `Save failed (${res.status}).`));
      const updated = (await res.json()) as Me;
      setMe(updated);
      setDraft("");
      setNotice(
        updated.team_id
          ? `You’re in “${updated.team_id}”. Teammates get added by an admin in Settings → Admin → Users.`
          : "You left your team. Team-shared projects are no longer visible to you, and the chats you shared into them are no longer shared.",
      );
      // The PUT echoes the account row only — no shared_projects/shared_chats.
      // Re-read GET /api/me/team so the Leave confirm keeps quoting real
      // counts instead of degrading to "we couldn't work out the numbers".
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to save your team.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <SetSection
      title="Team"
      intro="A team is the trust group that shared projects and shared conversations travel in. Everyone with the same team name can open the projects shared with it."
    >
      {error ? (
        <NoticeBanner tone="danger" className="mb-[0.85rem]">
          {error}
        </NoticeBanner>
      ) : null}
      {notice ? (
        <NoticeBanner tone="success" className="mb-[0.85rem]">
          {notice}
        </NoticeBanner>
      ) : null}

      <ConnPanel>
        <ConnPanelHead title="Your team" />
        <ConnPanelSub>
          Nothing is shared automatically: a conversation reaches your team only when you
          share it, and a project only when its owner marks it team-shared.
        </ConnPanelSub>

        {loading ? (
          <p className="text-[0.82rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : me?.team_id ? (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
              You are in{" "}
              <strong className="font-semibold" data-testid="team-current">
                {me.team_id}
              </strong>
              .
            </p>
            <button
              type="button"
              className={btnClass({ sm: true, danger: true })}
              disabled={busy}
              onClick={() => setConfirmLeave(true)}
            >
              Leave team
            </button>
          </div>
        ) : (
          <div className="grid gap-[0.55rem]">
            <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
              You are not in a team yet. Name a team to create it. Teammates are added by an
              admin in Settings → Admin → Users.
            </p>
            <div className="flex flex-wrap items-center gap-[0.55rem]">
              <input
                className={`${SETTINGS_INPUT} max-w-[18rem] flex-1`}
                placeholder="platform"
                aria-label="Team name"
                maxLength={64}
                value={draft}
                disabled={busy}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && draft.trim() && !busy) void write(draft.trim());
                }}
              />
              <button
                type="button"
                className={btnClass({ variant: "primary", sm: true })}
                disabled={busy || !draft.trim()}
                onClick={() => void write(draft.trim())}
              >
                {busy ? "Saving…" : me?.admin ? "Set team" : "Create team"}
              </button>
            </div>
            <p className="m-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
              {me?.admin
                ? "As an admin you can also put yourself (or anyone else) into a team that already exists, from Settings → Admin → Users."
                : "A name that another member already uses can’t be claimed here — ask an admin to add you to that team from Settings → Admin → Users."}
            </p>
          </div>
        )}
      </ConnPanel>

      <ConnPanel>
        <ConnPanelHead title="What a team unlocks" />
        <ul className="m-0 grid list-disc gap-[0.4rem] pl-[1.1rem] text-[0.82rem] leading-[1.55] text-[var(--color-text-secondary)]">
          <li>
            <strong className="font-semibold text-[var(--color-text-primary)]">
              Shared projects.
            </strong>{" "}
            Mark a project team-shared and every member can chat in it and read/write its
            shared memory. Only the owner edits or deletes the project itself.
          </li>
          <li>
            <strong className="font-semibold text-[var(--color-text-primary)]">
              Shared conversations.
            </strong>{" "}
            A conversation stays private until you share it with your team from its own
            menu <strong className="font-semibold">inside a team-shared project</strong>;
            teammates then find it on that project&rsquo;s home page and get a read-only
            view they can branch from.
          </li>
          <li>
            Your chats, memories, and connectors stay yours — a team never exposes them on
            its own.
          </li>
        </ul>
        <p className="mb-0 mt-[0.7rem] text-[0.78rem] text-[var(--color-text-muted)]">
          Start a shared workspace from{" "}
          <Link href="/chat" className="underline">
            chat → Projects
          </Link>
          .
        </p>
      </ConnPanel>

      {confirmLeave && me?.team_id ? (
        <LeaveTeamConfirm
          team={me.team_id}
          sharedProjects={me.shared_projects}
          sharedChats={me.shared_chats}
          busy={busy}
          onCancel={() => setConfirmLeave(false)}
          onConfirm={() => {
            setConfirmLeave(false);
            void write("");
          }}
        />
      ) : null}
    </SetSection>
  );
}

// LeaveTeamConfirm states what leaving costs BEFORE it happens (Item A4).
// Three facts, each verified against the code that implements it:
//
//   - the team-shared projects you stop seeing (those you do not own),
//   - the chats you shared into them, which leaving unshares — a team-shared
//     chat cannot exist without a place teammates would find it (ADR-0057),
//   - projects you OWN stay yours and stay shared with the team: ownership
//     is not team membership, and store.ListProjectsForUser matches the owner
//     regardless of team, so nothing about them changes.
function LeaveTeamConfirm({
  team,
  sharedProjects,
  sharedChats,
  busy,
  onCancel,
  onConfirm,
}: {
  team: string;
  sharedProjects?: number;
  sharedChats?: number;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const projects =
    sharedProjects === undefined
      ? "any project shared with it"
      : sharedProjects === 0
        ? "any project shared with it (there are none right now)"
        : `${sharedProjects} team-shared project${sharedProjects === 1 ? "" : "s"}`;
  return (
    <DialogShell
      label={`Leave ${team}?`}
      scrimLabel="Cancel leaving the team"
      onDismiss={onCancel}
      className="max-w-[26rem] p-5"
    >
      <h2 className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        Leave {team}?
      </h2>
      <ul className="mb-4 grid list-disc gap-[0.35rem] pl-[1.1rem] text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
        <li>You&rsquo;ll lose access to {projects}.</li>
        {sharedChats === undefined || sharedChats > 0 ? (
          <li>
            {sharedChats === undefined
              ? "Chats you shared with the team stop being shared"
              : `${sharedChats} chat${sharedChats === 1 ? "" : "s"} you shared with the team stop${sharedChats === 1 ? "s" : ""} being shared`}
            {" "}— they stay yours, teammates just can&rsquo;t open them any more.
          </li>
        ) : null}
        <li>
          Chats you filed in {team}&rsquo;s projects stay yours, but move back
          to Temporary — pin the ones you want to keep.
        </li>
        <li>Projects you own stay yours, and stay shared with {team}.</li>
      </ul>
      {sharedProjects === undefined || sharedChats === undefined ? (
        <p className="mb-4 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
          We couldn&rsquo;t work out the exact numbers just now, so they
          aren&rsquo;t shown above.
        </p>
      ) : null}
      <div className="flex items-center justify-end gap-2">
        <button
          type="button"
          className={btnClass({ sm: true })}
          onClick={onCancel}
        >
          Cancel
        </button>
        <button
          type="button"
          className={btnClass({ sm: true, danger: true })}
          disabled={busy}
          onClick={onConfirm}
        >
          Leave team
        </button>
      </div>
    </DialogShell>
  );
}

async function readError(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  return text.length > 0 && text.length <= 300 ? text : fallback;
}
