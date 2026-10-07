"use client";

import { useEffect, useId, useRef, useState } from "react";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { useChatToast } from "./ChatToasts";
import { LockGlyph } from "./ShareGlyphs";
import { ProjectVisibilityChoice } from "./NewProjectDialog";
import type { Project } from "./ProjectsModal";
import { plural } from "./teamSharing";

// Project settings (B26–B31, decisions #48–#51). OWNER only: the gear on the
// project home and the rail project menu are the two ways in, and both are
// shown to the owner alone.
//
// One dialog, three panels below the shared fields:
//   • the footer (Delete project · Cancel · Save) — Save applies the name and
//     "Who can see it" together;
//   • Delete (B29) — inline, with real counts from /impact and Export first;
//   • Transfer (B30/B31) — a member picker inside settings. It happens right
//     away, separately from Save, and is unavailable until the project is
//     shared (only a team member can become the owner).
//
// Switching a shared project to Only you is not a visibility tweak: it
// unshares every shared chat and moves teammates' chats out to their unfiled
// chats. The line under the choice says so with real counts BEFORE Save (B28)
// rather than in a second confirm afterwards.

// GET /api/projects/{id}/impact (internal/store/team_sharing.go ProjectImpact).
export type ProjectImpact = {
  memories: number;
  chats: number;
  members: number;
  team_shared_chats: number;
  // Chats in this project owned by somebody OTHER than the owner: what making
  // the project personal unfiles. OPTIONAL and nullable on purpose — the
  // store's LeaveTeamImpact sets the precedent (internal/store/team_sharing.go):
  // "we could not work out what this costs you" must not render as "nothing".
  chats_from_teammates?: number | null;
  teammates_with_chats?: number | null;
};

// The B28 line: what making a shared project personal costs, in one sentence.
// null impact = we couldn't count, and the sentence says so instead of "0".
export function makePrivateImpactText(impact: ProjectImpact | null): string {
  if (!impact)
    return "Shared chats will become Only you, and chats from teammates will move to their unfiled chats, where they can expire. We couldn’t count them.";
  const parts: string[] = [];
  if (impact.team_shared_chats > 0)
    parts.push(
      `${plural(impact.team_shared_chats, "shared chat", "shared chats")} will become Only you`,
    );
  const mates = impact.chats_from_teammates;
  const people = impact.teammates_with_chats;
  if (typeof mates === "number" && mates > 0)
    parts.push(
      `${plural(mates, "chat", "chats")} from ${
        typeof people === "number" && people > 0
          ? plural(people, "teammate", "teammates")
          : "teammates"
      } will move to their unfiled chats, where they can expire`,
    );
  else if (typeof mates !== "number")
    // An older server, or a count it could not work out: say so rather than
    // letting a missing field read as "no teammates' chats move".
    parts.push(
      "chats from teammates will move to their unfiled chats, where they can expire (we couldn’t count those)",
    );
  if (parts.length === 0)
    return "Only you will see this project. No chats are shared in it, so nothing else changes.";
  const text = parts.join(", and ");
  return `${text[0].toUpperCase()}${text.slice(1)}.`;
}

// The B29 list's first line.
export function deleteChatsText(impact: ProjectImpact | null): string {
  if (!impact)
    return "Every chat in it leaves the project and becomes temporary. They expire unless someone pins them.";
  if (impact.chats === 0) return "No chats are filed in it, so none become temporary.";
  return `${plural(impact.chats, "chat", "chats")} from ${plural(
    Math.max(impact.members, 1),
    "person",
    "people",
  )} leave the project and become temporary. They expire unless someone pins them.`;
}

// The /impact read both delete confirms (settings panel, rail kebab) and the
// B28 line share. settled=false until it lands; impact=null = couldn't count.
// Every new enabled read starts unsettled again: a count from an earlier
// opening (Only you → back → Only you) is stale, and a confirm gated on
// "settled" must wait for the fresh one rather than reuse it. The reset runs
// in the read's cleanup — whenever it is disabled or re-keyed — so the next
// enabled read begins from {settled:false} without a set-state in the body.
export function useImpact(projectId: string, enabled: boolean) {
  const [state, setState] = useState<{
    settled: boolean;
    impact: ProjectImpact | null;
  }>({ settled: false, impact: null });
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    void (async () => {
      try {
        const res = await fetch(
          `/api/projects/${encodeURIComponent(projectId)}/impact`,
          { cache: "no-store" },
        );
        const impact = res.ok ? ((await res.json()) as ProjectImpact) : null;
        if (!cancelled) setState({ settled: true, impact });
      } catch {
        if (!cancelled) setState({ settled: true, impact: null });
      }
    })();
    return () => {
      cancelled = true;
      setState({ settled: false, impact: null });
    };
  }, [projectId, enabled]);
  return state;
}

// Everyone who could own the project: its team plus the current owner
// (GET /projects/{id}/members, owner/admin only). null = not loaded.
function useMembers(projectId: string, enabled: boolean, nonce: number) {
  const [state, setState] = useState<{
    members: string[] | null;
    error: string | null;
  }>({ members: null, error: null });
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    void (async () => {
      try {
        const res = await fetch(
          `/api/projects/${encodeURIComponent(projectId)}/members`,
          { cache: "no-store" },
        );
        if (!res.ok) {
          if (!cancelled)
            setState({
              members: [],
              error: `Couldn’t load this project’s members (HTTP ${res.status}).`,
            });
          return;
        }
        const data = (await res.json()) as { members?: string[] };
        if (!cancelled) setState({ members: data.members ?? [], error: null });
      } catch {
        if (!cancelled)
          setState({
            members: [],
            error: "Couldn’t reach the server to list this project’s members.",
          });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [projectId, enabled, nonce]);
  return state;
}

const pillGhost =
  "rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50";
const pillPrimary =
  "rounded-full bg-[var(--color-primary)] px-4 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-on-primary)] transition hover:opacity-90 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50";
const pillDanger =
  "rounded-full bg-[var(--color-danger)] px-4 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-surface-1)] transition hover:opacity-90 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50";

export function ProjectSettingsDialog({
  project,
  myTeam,
  isAdmin,
  onClose,
  onSave,
  onTransfer,
  onDelete,
}: {
  project: Project;
  // The viewer's team: "" = none, undefined = not read yet.
  myTeam: string | undefined;
  isAdmin?: boolean;
  onClose: () => void;
  // PATCH passthrough; resolves true on success (the parent toasts failures).
  onSave: (patch: { name?: string; team_shared?: boolean }) => Promise<boolean>;
  // Resolves null on success, or the server's reason.
  onTransfer: (toEmail: string) => Promise<string | null>;
  onDelete: () => void;
}) {
  const { notify } = useChatToast();
  const wasShared = Boolean(project.team_id);
  // The owner keeps the project after a transfer only through its team: an
  // owner who has left that team (the case transfer exists for) loses access,
  // and the server moves their chats in it back to temporary.
  const ownerStaysMember = !wasShared || myTeam === project.team_id;
  // The team this project is (or would be) shared with. A shared project
  // names its own team — the owner may since have moved (C-9).
  const team = wasShared ? project.team_id : myTeam;
  const [name, setName] = useState(project.name);
  const [shared, setShared] = useState(wasShared);
  const [panel, setPanel] = useState<null | "delete" | "transfer">(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pick, setPick] = useState("");
  const [transferBusy, setTransferBusy] = useState(false);
  const [transferError, setTransferError] = useState<string | null>(null);
  const [membersNonce, setMembersNonce] = useState(0);
  const nameId = useId();
  const nameRef = useRef<HTMLInputElement | null>(null);

  const unsharing = wasShared && !shared;
  const impactState = useImpact(project.id, unsharing || panel === "delete");
  // Members: for the "<team> · N people" count and the transfer picker.
  // Only a shared project has a team roster to read.
  const membersState = useMembers(project.id, wasShared, membersNonce);
  const candidates = (membersState.members ?? []).filter(
    (m) => m.toLowerCase() !== project.owner_email.toLowerCase(),
  );
  const peopleCount =
    wasShared && membersState.members && !membersState.error
      ? membersState.members.length
      : undefined;
  const canTransfer = wasShared;

  const short = project.name;

  const save = async () => {
    if (saving) return;
    setError(null);
    const trimmed = name.trim();
    if (!trimmed) {
      setError("A project needs a name.");
      return;
    }
    const patch: { name?: string; team_shared?: boolean } = {};
    if (trimmed !== project.name) patch.name = trimmed;
    if (shared !== wasShared) patch.team_shared = shared;
    if (Object.keys(patch).length === 0) {
      onClose();
      return;
    }
    setSaving(true);
    const ok = await onSave(patch);
    setSaving(false);
    if (!ok) return;
    onClose();
    if (shared && !wasShared)
      notify({ message: `Shared with ${team || "your team"}. Chats stay Only you.` });
    else if (!shared && wasShared)
      notify({ message: "Project is Only you again." });
    else notify({ message: "Project settings saved." });
  };

  const transfer = async () => {
    if (!pick || transferBusy) return;
    setTransferBusy(true);
    setTransferError(null);
    const err = await onTransfer(pick);
    setTransferBusy(false);
    if (err) {
      setTransferError(err);
      return;
    }
    onClose();
    notify({ message: `${pick} now owns ${project.name}.` });
  };

  let note: string;
  let noteTone: "default" | "warning" = "default";
  if (shared) {
    note = `${team || "Your team"} will see the instructions, Team learnings, and chats shared with them. Each chat stays Only you until its owner shares it.`;
  } else if (unsharing) {
    noteTone = "warning";
    note = impactState.settled
      ? makePrivateImpactText(impactState.impact)
      : "Counting what changes…";
  } else {
    note = "Only you can see this project and its chats.";
  }

  return (
    <DialogShell
      label={`Settings for ${project.name}`}
      scrimLabel="Close project settings"
      onDismiss={onClose}
      initialFocusRef={nameRef}
      className="flex max-h-[90vh] max-w-[30rem] flex-col gap-4 overflow-y-auto p-5"
      testId="project-settings-dialog"
    >
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-[1.05rem] font-semibold text-[var(--color-text-primary)]">
          Project settings
        </h2>
        <CloseButton label="Close project settings" onClick={onClose} />
      </div>

      <div className="grid gap-1.5">
        <label
          htmlFor={nameId}
          className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
        >
          Name
        </label>
        <input
          id={nameId}
          ref={nameRef}
          value={name}
          maxLength={128}
          // "Project name" stays the accessible name the e2e specs query by.
          aria-label="Project name"
          onChange={(e) => setName(e.target.value)}
          className="w-full rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
        />
      </div>

      <ProjectVisibilityChoice
        // A shared project's team option is always available (it IS the
        // current state); otherwise it follows the viewer's own team.
        team={wasShared ? project.team_id : myTeam}
        teamShared={shared}
        peopleCount={peopleCount}
        isAdmin={isAdmin}
        note={note}
        noteTone={noteTone}
        onChange={setShared}
      />

      <div className="flex items-center gap-3 rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-2.5 text-[0.8rem] leading-[1.45] text-[var(--color-text-secondary)]">
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span>
            Owner:{" "}
            <span className="text-[var(--color-text-primary)]">
              {project.owner_email}
            </span>
            . Only the owner can edit, share, or delete.
          </span>
          {canTransfer ? null : (
            <span className="text-[var(--color-text-muted)]">
              Transfer needs a member, so share the project first.
            </span>
          )}
        </span>
        {canTransfer ? (
          <button
            type="button"
            aria-expanded={panel === "transfer"}
            className="shrink-0 rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.78rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={() => {
              setPanel(panel === "transfer" ? null : "transfer");
              setTransferError(null);
            }}
          >
            Transfer…
          </button>
        ) : (
          <button
            type="button"
            disabled
            aria-disabled="true"
            className="inline-flex shrink-0 cursor-not-allowed items-center gap-1 rounded-full border border-dashed border-[var(--color-border-strong)] px-3 py-1 text-[0.78rem] font-medium text-[var(--color-text-disabled)]"
          >
            <LockGlyph className="size-3" />
            Transfer… (unavailable)
          </button>
        )}
      </div>

      {panel === "transfer" ? (
        <div
          role="group"
          aria-label="Transfer ownership"
          className="grid gap-2.5 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] p-3"
        >
          <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
            Transfer ownership
          </span>
          {membersState.members === null ? (
            <p className="m-0 text-[0.78rem] text-[var(--color-text-muted)]">
              Loading members…
            </p>
          ) : membersState.error ? (
            <p className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-danger)]">
              {membersState.error}{" "}
              <button
                type="button"
                className="underline"
                onClick={() => setMembersNonce((n) => n + 1)}
              >
                Try again
              </button>
            </p>
          ) : candidates.length === 0 ? (
            <p className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-text-secondary)]">
              Only a member can become the owner, and nobody else is on{" "}
              {team || "this project’s team"} yet.
            </p>
          ) : (
            <>
              <div role="radiogroup" aria-label="New owner" className="grid gap-1.5">
                {candidates.map((m) => (
                  <button
                    key={m}
                    type="button"
                    role="radio"
                    aria-checked={pick === m}
                    onClick={() => setPick(m)}
                    className={[
                      "flex items-center gap-2.5 rounded-[var(--radius-md)] border px-2.5 py-2 text-left text-[0.82rem] text-[var(--color-text-primary)] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                      pick === m
                        ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)]"
                        : "border-[var(--color-border)] hover:bg-[var(--color-overlay-soft)]",
                    ].join(" ")}
                  >
                    <span
                      aria-hidden="true"
                      className="grid size-6 shrink-0 place-items-center rounded-full bg-[var(--color-overlay-strong)] text-[0.72rem] font-semibold uppercase text-[var(--color-text-secondary)]"
                    >
                      {m.charAt(0)}
                    </span>
                    <span className="min-w-0 flex-1 truncate">{m}</span>
                  </button>
                ))}
              </div>
              <p className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-text-secondary)]">
                {pick
                  ? ownerStaysMember
                    ? `${pick} becomes the owner. You stay a member: you keep your chats here and can still share them, but you can’t edit settings, share the project, or delete it. This happens right away, separately from Save.`
                    : `${pick} becomes the owner. You’re no longer on ${project.team_id}, so you lose access to ${short}, and your chats in it become temporary again. This happens right away, separately from Save.`
                  : `Pick who should own ${short}.`}
              </p>
            </>
          )}
          {transferError ? (
            <p role="alert" className="m-0 text-[0.78rem] text-[var(--color-danger)]">
              {transferError}
            </p>
          ) : null}
          <div className="flex justify-end gap-2">
            <button
              type="button"
              className={pillGhost}
              onClick={() => setPanel(null)}
            >
              Cancel
            </button>
            {candidates.length > 0 ? (
              <button
                type="button"
                disabled={!pick || transferBusy}
                className={pillPrimary}
                onClick={() => void transfer()}
              >
                {transferBusy ? "Transferring…" : "Transfer ownership"}
              </button>
            ) : null}
          </div>
        </div>
      ) : null}

      {panel === "delete" ? (
        <div
          role="alertdialog"
          aria-label={`Delete ${short}?`}
          className="grid gap-2.5 rounded-[var(--radius-lg)] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_8%,transparent)] p-3"
        >
          <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
            Delete {short}?
          </span>
          <ul className="m-0 grid list-disc gap-1 pl-[1.1rem] text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
            <li>
              {impactState.settled
                ? deleteChatsText(impactState.impact)
                : "Counting the chats in it…"}
            </li>
            <li>
              {impactState.impact
                ? `Team learnings (${impactState.impact.memories}) are lost.`
                : "Its team learnings are lost."}
            </li>
            <li>Instructions and sharing are removed.</li>
          </ul>
          <div className="flex flex-wrap items-center gap-2">
            <a
              href={`/api/projects/${encodeURIComponent(project.id)}/export`}
              className={pillGhost}
            >
              Export first
            </a>
            <span className="flex-1" />
            <button
              type="button"
              className={pillGhost}
              onClick={() => setPanel(null)}
            >
              Cancel
            </button>
            <button
              type="button"
              // The list above promises real counts; the delete waits until
              // they land (a failed count still settles, with honest wording).
              disabled={!impactState.settled}
              className={pillDanger}
              onClick={() => {
                if (!impactState.settled) return;
                onClose();
                onDelete();
              }}
            >
              Delete project
            </button>
          </div>
        </div>
      ) : null}

      {error ? (
        <p role="alert" className="m-0 text-[0.78rem] text-[var(--color-danger)]">
          {error}
        </p>
      ) : null}

      {panel === null ? (
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="rounded-[var(--radius-md)] px-2 py-1.5 text-[0.8125rem] font-medium text-[var(--color-danger)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={() => setPanel("delete")}
          >
            Delete project
          </button>
          <span className="flex-1" />
          <button type="button" className={pillGhost} onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            // Switching to Only you promises the counts BEFORE Save (B28), so
            // Save waits until they land; a failed count still settles, with
            // its honest "couldn't count" wording.
            disabled={saving || (unsharing && !impactState.settled)}
            className={pillPrimary}
            onClick={() => void save()}
          >
            {saving ? "Saving…" : "Save"}
          </button>
        </div>
      ) : null}
    </DialogShell>
  );
}
