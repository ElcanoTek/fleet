import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
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

import { useCallback, useState } from "react";
import { getBootstrap } from "@/app/lib/bootstrap";
import { Link } from "@/app/lib/Link";

import { btnClass, SETTINGS_INPUT } from "../ui/atoms";
import { ConnPanel, ConnPanelHead, ConnPanelSub, SetSection } from "../ui/panels";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { responseErrorText } from "@/app/lib/responseError";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { invalidateMe } from "@/app/shared/lib/account";
import { redirectToLogin } from "@/app/settings/lib/getGated";
import { plural } from "@/app/shared/lib/format";

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
  const [draft, setDraft] = useState("");
  // Write failures; a load failure is the fetch hook's own error below.
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Set when central Auth manages this Fleet's teams (team sync): Auth sends
  // each person's team, so this page shows it but cannot change it (the
  // server refuses PUT /api/me/team there too).
  const centralAuth = getBootstrap().centralAuth;
  // Leaving is destructive in a way the old fire-then-report flow never said:
  // it unshares chats. true = the confirm is open.
  const [confirmLeave, setConfirmLeave] = useState(false);

  // The account and its leave impact, read through useCancellableFetch
  // (#2009): leaving the page aborts a read still in flight. A 401 goes to
  // sign-in and leaves the record empty.
  const {
    data: me,
    setData: setMe,
    loading: reading,
    error: loadError,
    reload: load,
  } = useCancellableFetch(
    useCallback(async (signal: AbortSignal): Promise<Me | null> => {
      const res = await fetch("/api/me/team", { cache: "no-store", signal });
      if (res.status === 401) {
        redirectToLogin();
        return null;
      }
      if (!res.ok) throw new Error(await responseErrorText(res, tr("Request failed")));
      return (await res.json()) as Me;
    }, []),
    [],
  );
  // Only the first read shows the loading state; the re-read after a save
  // keeps the form on screen.
  const loading = reading && me === null;
  const shownError = error ?? loadError;

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
        redirectToLogin();
        return;
      }
      if (res.status === 409) {
        // The server refuses a name that is already in use — by another
        // member OR by a team-shared project whose team has no members left
        // (ADR-0047). It cannot say which, so neither does this: the fix is
        // the same either way.
        throw new Error(
          tr("That name is already in use. An admin can add you to the team in Settings → Admin → Users."),
        );
      }
      if (!res.ok) throw new Error(await responseErrorText(res, tr("Save failed")));
      const updated = (await res.json()) as Me;
      // Every other reader shares one cached account record; it now names
      // the old team.
      invalidateMe();
      setMe(updated);
      setDraft("");
      setNotice(
        updated.team_id
          ? tr("You’re in “{0}”. Teammates get added by an admin in Settings → Admin → Users.", [updated.team_id])
          : tr("You left your team. Team-shared projects are no longer visible to you, and the chats you shared into them are no longer shared."),
      );
      // The PUT echoes the account row only — no shared_projects/shared_chats.
      // Re-read GET /api/me/team so the Leave confirm keeps quoting real
      // counts instead of degrading to "we couldn't work out the numbers".
      await load();
    } catch (err) {
      setError(errorMessage(err, tr("Unable to save your team.")));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SetSection
      title={tr("Team")}
      intro={tr("A team is the trust group that shared projects and shared conversations travel in. Everyone with the same team name can open the projects shared with it.")}
    >
      {shownError ? (
        <NoticeBanner tone="danger" className="mb-[0.85rem]">
          {shownError}
        </NoticeBanner>
      ) : null}
      {notice ? (
        <NoticeBanner tone="success" className="mb-[0.85rem]">
          {notice}
        </NoticeBanner>
      ) : null}

      <ConnPanel>
        <ConnPanelHead title={tr("Your team")} />
        <ConnPanelSub>
          {tr("Nothing is shared automatically: a conversation reaches your team only when you share it, and a project only when its owner marks it team-shared.")}
        </ConnPanelSub>

        {loading ? (
          <p className="text-[0.82rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
        ) : centralAuth ? (
          <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
        {rich("{0}Teams are set in Auth by an administrator.", me?.team_id ? (
              <>
                {tr("You are in")}{" "}
                <strong className="font-semibold" data-testid="team-current">
                  {me.team_id}
                </strong>
                .{" "}
              </>
            ) : (
              tr("You are not in a team. ")
            ))}
      </p>
        ) : me?.team_id ? (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
        {rich("You are in {0}.", <strong className="font-semibold" data-testid="team-current">
                {me.team_id}
              </strong>)}
      </p>
            <button
              type="button"
              className={btnClass({ sm: true, danger: true })}
              disabled={busy}
              onClick={() => setConfirmLeave(true)}
            >
              {tr("Leave team")}
            </button>
          </div>
        ) : (
          <div className="grid gap-[0.55rem]">
            <p className="m-0 text-[0.9rem] text-[var(--color-text-primary)]">
              {tr("You are not in a team yet. Name a team to create it. Teammates are added by an admin in Settings → Admin → Users.")}
            </p>
            <div className="flex flex-wrap items-center gap-[0.55rem]">
              <input
                id="teamName"
                className={`${SETTINGS_INPUT} max-w-[18rem] flex-1`}
                placeholder={tr("platform")}
                aria-label={tr("Team name")}
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
                {busy ? tr("Saving…") : me?.admin ? tr("Set team") : tr("Create team")}
              </button>
            </div>
            <p className="m-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
              {me?.admin
                ? tr("As an admin you can also put yourself (or anyone else) into a team that already exists, from Settings → Admin → Users.")
                : tr("A name that another member already uses can’t be claimed here — ask an admin to add you to that team from Settings → Admin → Users.")}
            </p>
          </div>
        )}
      </ConnPanel>

      <ConnPanel>
        <ConnPanelHead title={tr("What a team unlocks")} />
        <ul className="m-0 grid list-disc gap-[0.4rem] pl-[1.1rem] text-[0.82rem] leading-[1.55] text-[var(--color-text-secondary)]">
          <li>
        {rich("{0} Mark a project team-shared and every member can chat in it and read/write its shared memory. Only the owner edits or deletes the project itself.", <strong className="font-semibold text-[var(--color-text-primary)]">
              {tr("Shared projects.")}
            </strong>)}
      </li>
          <li>
        {rich("{0} A conversation stays private until you share it with your team from its own menu {1}; teammates then find it on that project’s home page and get a read-only view they can branch from.", <strong className="font-semibold text-[var(--color-text-primary)]">
              {tr("Shared conversations.")}
            </strong>, <strong className="font-semibold">{tr("inside a team-shared project")}</strong>)}
      </li>
          <li>
            {tr("Your chats, memories, and connectors stay yours — a team never exposes them on its own.")}
          </li>
        </ul>
        <p className="mb-0 mt-[0.7rem] text-[0.78rem] text-[var(--color-text-muted)]">
          {tr("Start a shared workspace from")}{" "}
          <Link href="/chat" className="underline">
            {tr("chat → Projects")}
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
      ? tr("any project shared with it")
      : sharedProjects === 0
        ? tr("any project shared with it (there are none right now)")
        : plural(sharedProjects, "team-shared project");
  return (
    <DialogShell
      label={tr("Leave {0}?", [team])}
      scrimLabel={tr("Cancel leaving the team")}
      onDismiss={onCancel}
      className="max-w-[26rem] p-5"
    >
      <h2 className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        {rich("Leave {0}?", team)}
      </h2>
      <ul className="mb-4 grid list-disc gap-[0.35rem] pl-[1.1rem] text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
        <li>
        {rich("You’ll lose access to {0}.", projects)}
      </li>
        {sharedChats === undefined || sharedChats > 0 ? (
          <li>
        {rich("{0} — they stay yours, teammates just can’t open them any more.", sharedChats === undefined
              ? tr("Chats you shared with the team stop being shared")
              : tr(sharedChats === 1 ? "{0} chat you shared with the team stops being shared" : "{0} chats you shared with the team stop being shared", [sharedChats]))}
      </li>
        ) : null}
        <li>
        {rich("Chats you filed in {0}’s projects stay yours, but move back to Temporary — pin the ones you want to keep.", team)}
      </li>
        <li>
        {rich("Projects you own stay yours, and stay shared with {0}.", team)}
      </li>
      </ul>
      {sharedProjects === undefined || sharedChats === undefined ? (
        <p className="mb-4 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
          {tr("We couldn’t work out the exact numbers just now, so they aren’t shown above.")}
        </p>
      ) : null}
      <div className="flex items-center justify-end gap-2">
        <button
          type="button"
          className={btnClass({ sm: true })}
          onClick={onCancel}
        >
          {tr("Cancel")}</button>
        <button
          type="button"
          className={btnClass({ sm: true, danger: true })}
          disabled={busy}
          onClick={onConfirm}
        >
          {tr("Leave team")}
        </button>
      </div>
    </DialogShell>
  );
}
