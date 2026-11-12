import { t as tr } from "@/app/lib/i18n";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
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
  const head = tr("You’re not on a team, so projects stay Only you.");
  if (isAdmin === true)
    return tr("{0} Add yourself in Settings → Admin → Users, or create a team in Settings → Team.", [head]);
  if (isAdmin === false)
    return tr("{0} Ask an admin to add you in Settings → Admin → Users.", [head]);
  return tr("{0} Admins: add yourself in Settings → Admin → Users, or create a team in Settings → Team. Everyone else: ask an admin.", [head]);
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
        {tr("Who can see it")}
      </span>
      <div
        role="radiogroup"
        aria-label={tr("Who can see this project")}
        aria-describedby={noteId}
        className="flex flex-col gap-2 sm:flex-row"
      >
        <Option
          checked={!teamShared}
          icon={<LockGlyph className="size-4" />}
          title={tr("Only you")}
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
          title={noTeam ? tr("Team (unavailable)") : team || tr("Team")}
          sub={
            noTeam
              ? tr("You’re not on a team")
              : typeof peopleCount === "number"
                ? tr(peopleCount === 1 ? "{0} person" : "{0} people", [peopleCount])
                : tr("Your team")
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
  const teamName = team || tr("your team");
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
      label={tr("New project")}
      scrimLabel={tr("Close new project")}
      onDismiss={onClose}
      initialFocusRef={nameRef}
      className="flex max-h-[88vh] max-w-[30rem] flex-col gap-4 overflow-y-auto p-5"
      testId="new-project-dialog"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-[1.05rem] font-semibold text-[var(--color-text-primary)]">
            {tr("New project")}
          </h2>
          <p className="mt-0.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
            {tr("A home for related chats, with shared instructions and memory.")}
          </p>
        </div>
        <CloseButton label={tr("Close new project")} onClick={onClose} />
      </div>

      {moveChat ? (
        <p className="m-0 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {shared
            ? tr("“{0}” moves into the new project and is shared with {1}.", [moveChat.title, teamName])
            : tr("“{0}” moves into the new project.", [moveChat.title])}
        </p>
      ) : null}

      <div className="grid gap-1.5">
        <Label
          htmlFor={nameId}
          className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
        >
          {tr("Name")}
        </Label>
        <Input
          id={nameId}
          ref={nameRef}
          value={name}
          maxLength={128}
          placeholder={tr("e.g. Knowertech: Q4 planning")}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void create();
            }
          }}
        />
      </div>

      <ProjectVisibilityChoice
        team={team}
        teamShared={shared}
        isAdmin={isAdmin}
        note={
          shared
            ? tr("{0} will see the instructions and Team learnings. Each chat stays Only you until its owner shares it.", [teamName])
            : tr("Only you can see it. You can share it with {0} any time.", [teamName])
        }
        onChange={setTeamShared}
      />

      {showInstructions ? (
        <div className="grid gap-1.5">
          <Label
            htmlFor={instructionsId}
            className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
          >
            {tr("Instructions (optional)")}
          </Label>
          <Textarea
            id={instructionsId}
            ref={instructionsRef}
            value={instructions}
            onChange={(e) => setInstructions(e.target.value)}
            placeholder={tr("Standing instructions every chat here follows…")}
            className="min-h-24 resize-y leading-[1.5]"
          />
        </div>
      ) : (
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto self-start px-0 py-0 text-[0.82rem] text-[var(--color-accent)]"
          onClick={() => setShowInstructions(true)}
        >
          {tr("+ Add instructions (optional)")}
        </Button>
      )}

      {error ? (
        <p role="alert" className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-danger)]">
          {error}
        </p>
      ) : null}

      <div className="flex items-center justify-end gap-2">
        <Button
          type="button"
          variant="secondary"
          onClick={onClose}
        >
          {tr("Cancel")}</Button>
        <Button
          type="button"
          variant="default"
          disabled={!canCreate}
          onClick={() => void create()}
        >
          {busy ? tr("Creating…") : tr("Create project")}
        </Button>
      </div>
    </DialogShell>
  );
}
