"use client";

import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { LockGlyph, TeamGlyph } from "./ShareGlyphs";

// New project (B27, decision #52): a focused dialog — name, who can see it,
// optional instructions — instead of the all-projects modal's inline form.
// Settings and New project use the same "Only you / <team>" choice, so the
// radio pair lives here and ProjectSettingsDialog imports it.
//
// Opened two ways:
//   • plain (rail +, Projects modal "New project"): creating lands on the new
//     project's home — the caller decides that, this dialog only collects.
//   • from the share dialog's A1b state (`moveChat`): the team is
//     preselected and a line says the chat moves in and is shared; the caller
//     then moves the chat, shares it with its outputs, and keeps the user on
//     the chat.

// The A3a guidance for someone on no team, split by who can fix it: most
// people on a fleet box ARE admins, so telling them to ask someone else would
// be worse than useless. undefined = the admin read hasn't landed; then both
// paths are named.
function noTeamGuidance(isAdmin: boolean | undefined): string {
  const head = "You’re not on a team, so projects stay Only you.";
  if (isAdmin === true)
    return `${head} Add yourself in Settings → Admin → Users, or create a team in Settings → Team.`;
  if (isAdmin === false)
    return `${head} Ask an admin to add you in Settings → Admin → Users.`;
  return `${head} Admins: add yourself in Settings → Admin → Users, or create a team in Settings → Team. Everyone else: ask an admin.`;
}

function Radio({ on }: { on: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={[
        "grid size-4 shrink-0 place-items-center rounded-full border",
        on
          ? "border-[var(--color-accent)]"
          : "border-[var(--color-border-strong)]",
      ].join(" ")}
    >
      {on ? (
        <span className="size-2 rounded-full bg-[var(--color-accent)]" />
      ) : null}
    </span>
  );
}

function Option({
  checked,
  disabled,
  icon,
  title,
  sub,
  onPick,
}: {
  checked: boolean;
  disabled?: boolean;
  icon: ReactNode;
  title: ReactNode;
  sub: ReactNode;
  onPick: () => void;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={checked}
      aria-disabled={disabled || undefined}
      disabled={disabled}
      onClick={onPick}
      className={[
        "flex min-w-0 flex-1 items-center gap-2.5 rounded-[var(--radius-md)] border px-3 py-2.5 text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
        disabled
          ? // The house "unavailable" treatment (A3a): dashed outline, a lock,
            // muted text — visibly there, visibly not for you right now.
            "cursor-not-allowed border-dashed border-[var(--color-border-strong)] text-[var(--color-text-disabled)]"
          : checked
            ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] text-[var(--color-text-primary)]"
            : "border-[var(--color-border)] text-[var(--color-text-primary)] hover:bg-[var(--color-overlay-soft)]",
      ].join(" ")}
    >
      <span className="shrink-0 text-[var(--color-text-secondary)]">{icon}</span>
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate text-[0.85rem] font-semibold">{title}</span>
        <span
          className={[
            "truncate text-[0.75rem]",
            disabled
              ? "text-[var(--color-text-disabled)]"
              : "text-[var(--color-text-muted)]",
          ].join(" ")}
        >
          {sub}
        </span>
      </span>
      {disabled ? null : <Radio on={checked} />}
    </button>
  );
}

// ProjectVisibilityChoice — "Who can see it": Only you · Private project, or
// <team> · N people. `team` "" = the viewer is on no team: the team option
// renders unavailable, with the reason, and `note` is replaced by the A3a
// guidance. `team` undefined = not read yet: the option is neutral ("Team").
export function ProjectVisibilityChoice({
  team,
  teamShared,
  peopleCount,
  note,
  noteTone = "default",
  isAdmin,
  onChange,
}: {
  team: string | undefined;
  teamShared: boolean;
  // Known size of the team, for "<team> · N people". undefined = unknown, and
  // no number is shown.
  peopleCount?: number;
  note?: ReactNode;
  noteTone?: "default" | "warning";
  isAdmin?: boolean;
  onChange: (teamShared: boolean) => void;
}) {
  const noTeam = team === "";
  const noteId = useId();
  return (
    <div className="grid gap-1.5">
      <span className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]">
        Who can see it
      </span>
      <div
        role="radiogroup"
        aria-label="Who can see this project"
        aria-describedby={noteId}
        className="flex flex-col gap-2 sm:flex-row"
      >
        <Option
          checked={!teamShared}
          icon={<LockGlyph className="size-4" />}
          title="Only you"
          sub="Private project"
          onPick={() => onChange(false)}
        />
        <Option
          checked={teamShared && !noTeam}
          disabled={noTeam}
          icon={
            noTeam ? (
              <LockGlyph className="size-4" />
            ) : (
              <TeamGlyph className="size-4" />
            )
          }
          title={noTeam ? "Team (unavailable)" : team || "Team"}
          sub={
            noTeam
              ? "You’re not on a team"
              : typeof peopleCount === "number"
                ? `${peopleCount} ${peopleCount === 1 ? "person" : "people"}`
                : "Your team"
          }
          onPick={() => onChange(true)}
        />
      </div>
      <p
        id={noteId}
        className={[
          "m-0 text-[0.78rem] leading-[1.5]",
          noteTone === "warning" && !noTeam
            ? "text-[var(--color-warning)]"
            : "text-[var(--color-text-secondary)]",
        ].join(" ")}
      >
        {noTeam ? noTeamGuidance(isAdmin) : note}
      </p>
    </div>
  );
}

export type NewProjectInput = {
  name: string;
  instructions: string;
  teamShared: boolean;
};

export function NewProjectDialog({
  team,
  isAdmin,
  teamPreselected,
  moveChat,
  onClose,
  onCreate,
}: {
  // The viewer's team: "" = none, undefined = not read yet.
  team: string | undefined;
  isAdmin?: boolean;
  teamPreselected?: boolean;
  moveChat?: { id: string; title: string };
  onClose: () => void;
  // Resolves null on success (the caller closes the dialog and navigates), or
  // a sentence to show when the create failed.
  onCreate: (input: NewProjectInput) => Promise<string | null>;
}) {
  const [name, setName] = useState("");
  const [teamShared, setTeamShared] = useState(
    Boolean(teamPreselected) && team !== "",
  );
  const [showInstructions, setShowInstructions] = useState(false);
  const [instructions, setInstructions] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const nameRef = useRef<HTMLInputElement | null>(null);
  const instructionsRef = useRef<HTMLTextAreaElement | null>(null);
  // "+ Add instructions" reveals the field and moves focus into it — tied to
  // that click, not to any mount.
  useEffect(() => {
    if (showInstructions) instructionsRef.current?.focus();
  }, [showInstructions]);
  const nameId = useId();
  const instructionsId = useId();
  const teamName = team || "your team";
  const shared = teamShared && team !== "";
  const canCreate = name.trim().length > 0 && !busy;

  const create = async () => {
    if (!canCreate) return;
    setBusy(true);
    setError(null);
    const err = await onCreate({
      name: name.trim(),
      instructions: instructions.trim(),
      teamShared: shared,
    });
    setBusy(false);
    if (err) setError(err);
  };

  return (
    <DialogShell
      label="New project"
      scrimLabel="Close new project"
      onDismiss={onClose}
      initialFocusRef={nameRef}
      className="flex max-h-[88vh] max-w-[30rem] flex-col gap-4 overflow-y-auto p-5"
      testId="new-project-dialog"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-[1.05rem] font-semibold text-[var(--color-text-primary)]">
            New project
          </h2>
          <p className="mt-0.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
            A home for related chats, with shared instructions and memory.
          </p>
        </div>
        <CloseButton label="Close new project" onClick={onClose} />
      </div>

      {moveChat ? (
        <p className="m-0 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {shared
            ? `“${moveChat.title}” moves into the new project and is shared with ${teamName}.`
            : `“${moveChat.title}” moves into the new project.`}
        </p>
      ) : null}

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
          placeholder="e.g. Knowertech: Q4 planning"
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void create();
            }
          }}
          className="w-full rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
        />
      </div>

      <ProjectVisibilityChoice
        team={team}
        teamShared={shared}
        isAdmin={isAdmin}
        note={
          shared
            ? `${teamName} will see the instructions and Team learnings. Each chat stays Only you until its owner shares it.`
            : `Only you can see it. You can share it with ${teamName} any time.`
        }
        onChange={setTeamShared}
      />

      {showInstructions ? (
        <div className="grid gap-1.5">
          <label
            htmlFor={instructionsId}
            className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
          >
            Instructions (optional)
          </label>
          <textarea
            id={instructionsId}
            ref={instructionsRef}
            value={instructions}
            onChange={(e) => setInstructions(e.target.value)}
            placeholder="Standing instructions every chat here follows…"
            className="min-h-24 w-full resize-y rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
          />
        </div>
      ) : (
        <button
          type="button"
          className="self-start rounded-[var(--radius-md)] text-[0.82rem] font-medium text-[var(--color-accent)] transition hover:underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={() => setShowInstructions(true)}
        >
          + Add instructions (optional)
        </button>
      )}

      {error ? (
        <p role="alert" className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-danger)]">
          {error}
        </p>
      ) : null}

      <div className="flex items-center justify-end gap-2">
        <button
          type="button"
          className="rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={onClose}
        >
          Cancel
        </button>
        <button
          type="button"
          disabled={!canCreate}
          className="rounded-full bg-[var(--color-primary)] px-4 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-on-primary)] transition hover:opacity-90 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
          onClick={() => void create()}
        >
          {busy ? "Creating…" : "Create project"}
        </button>
      </div>
    </DialogShell>
  );
}
