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

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";

import { btnClass, SETTINGS_INPUT } from "../ui/atoms";
import { ConnPanel, ConnPanelHead, ConnPanelSub, SetSection } from "../ui/panels";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

export type Me = {
  email: string;
  role: string;
  team_id: string;
  admin: boolean;
};

export default function TeamSettingsPage() {
  const [me, setMe] = useState<Me | null>(null);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);

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
      if (!res.ok) throw new Error(await readError(res, `Save failed (${res.status}).`));
      const updated = (await res.json()) as Me;
      setMe(updated);
      setDraft("");
      setNotice(
        updated.team_id
          ? `You are now in team “${updated.team_id}”. New projects can be shared with it.`
          : "You left your team. Team-shared projects are no longer visible to you.",
      );
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
              onClick={() => void write("")}
            >
              Leave team
            </button>
          </div>
        ) : (
          <div className="grid gap-[0.55rem]">
            <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
              You are not in a team yet. Name one to create it — everyone you want to share
              with then joins the same name.
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
            menu; teammates then get a read-only view.
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
    </SetSection>
  );
}

async function readError(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  return text.length > 0 && text.length <= 300 ? text : fallback;
}
