"use client";

import { Icon } from "./Icon";

// The project home's getting-started card (Fleet Projects sharing, B6 / B8 /
// B12 / B25; decisions #15, #16, #21). It replaces the old empty-state
// paragraphs with the next concrete step toward the team path, per person and
// per project, until that person has shared a chat here
// (my-state.has_shared_chat). Steps tick themselves off from live data and
// each open step carries the action that completes it, so the card is a list
// of buttons rather than a list of instructions.
//
// The variant model is a pure function (buildGettingStarted) so the four
// shapes are pinned by tests without rendering the whole home.

// What a step's button does. Named kinds, not callbacks, so the model stays
// plain data (it is built during render) and the page decides the effect.
export type StepActionKind =
  | "add-instructions"
  | "new-chat"
  | "share-project"
  | "share-ready-chat";

export type StepAction = {
  label: string;
  tone: "primary" | "secondary";
  kind: StepActionKind;
  busy?: boolean;
};

export type GettingStartedStep = {
  label: string;
  sub?: string;
  done?: boolean;
  action?: StepAction;
};

export type GettingStartedModel = {
  variant: "owner-shared" | "member-shared" | "owner-personal" | "no-team";
  title: string;
  lead: string;
  steps: GettingStartedStep[];
  // B12 only: dismiss the card for good for this project and person.
  canKeepPersonal: boolean;
};

export type GettingStartedInput = {
  projectName: string;
  // The team the project is shared with ("" = personal project).
  projectTeam: string;
  // The viewer's own team ("" = not on a team). Never undefined here — the
  // caller waits for the read before showing a card that depends on it.
  myTeam: string;
  isOwner: boolean;
  hasInstructions: boolean;
  myChatCount: number;
  myPrivateChatCount: number;
  // "Share project first" landed here (B15): the chat the owner came from,
  // when it is still Only you.
  readyChat?: { id: string; title: string } | null;
  busy?: { shareProject?: boolean; shareReadyChat?: boolean };
};

const INSTRUCTIONS_SUB = "Optional. Every chat here follows them.";

export function buildGettingStarted(i: GettingStartedInput): GettingStartedModel {
  const addInstructions: GettingStartedStep = {
    label: "Add instructions",
    done: i.hasInstructions,
    sub: i.hasInstructions ? undefined : INSTRUCTIONS_SUB,
    action: i.hasInstructions
      ? undefined
      : { label: "Add", tone: "secondary", kind: "add-instructions" },
  };

  // B25: no team, so there is nobody to share with — no share step at all.
  if (!i.myTeam && !i.projectTeam) {
    return {
      variant: "no-team",
      title: `Get started in ${i.projectName}`,
      lead: "Sharing needs a team. Admins: add yourself in Settings → Admin → Users, or create a team in Settings → Team. Everyone else: ask an admin.",
      steps: [
        addInstructions,
        {
          label: "Start a chat",
          done: i.myChatCount > 0,
          action:
            i.myChatCount > 0
              ? undefined
              : { label: "New chat", tone: "primary", kind: "new-chat" },
        },
      ],
      canKeepPersonal: false,
    };
  }

  const team = i.projectTeam || i.myTeam;
  const lead = `Chats start as Only you. Share the ones ${team} should read and build on.`;

  // B12: the owner's personal project — share the project first.
  if (!i.projectTeam) {
    return {
      variant: "owner-personal",
      title: `Work on this with ${team}`,
      lead: "Share the project first. Each chat stays Only you until you share it.",
      steps: [
        {
          label: `Share ${i.projectName} with ${team}`,
          sub: `${team} sees the instructions and Team learnings. Chats stay Only you.`,
          action: {
            label: "Share project",
            tone: "primary",
            kind: "share-project",
            busy: i.busy?.shareProject,
          },
        },
        addInstructions,
        { label: `Share a chat with ${team}`, sub: "Available after step 1" },
      ],
      canKeepPersonal: true,
    };
  }

  // B6: the owner of a shared project.
  if (i.isOwner) {
    let share: GettingStartedStep;
    if (i.readyChat) {
      share = {
        label: `Share a chat with ${team}`,
        sub: `“${i.readyChat.title}” is ready to share.`,
        action: {
          label: "Share it",
          tone: "primary",
          kind: "share-ready-chat",
          busy: i.busy?.shareReadyChat,
        },
      };
    } else if (i.myPrivateChatCount > 0) {
      share = {
        label: `Share a chat with ${team}`,
        sub: `Switch any chat below from Only you to ${team}.`,
      };
    } else {
      share = {
        label: `Share a chat with ${team}`,
        sub: `Start one, then switch it to ${team}.`,
        action: { label: "New chat", tone: "primary", kind: "new-chat" },
      };
    }
    return {
      variant: "owner-shared",
      title: `Get ${team} working here`,
      lead,
      steps: [{ label: `Project shared with ${team}`, done: true }, addInstructions, share],
      canKeepPersonal: false,
    };
  }

  // B8: a member of a shared project.
  const started = i.myChatCount > 0;
  return {
    variant: "member-shared",
    title: `Get started in ${i.projectName}`,
    lead,
    steps: [
      {
        label: "Start a chat here, or branch one",
        done: started,
        sub: started ? undefined : "You can also move a chat in from the sidebar.",
        action: started
          ? undefined
          : { label: "New chat", tone: "primary", kind: "new-chat" },
      },
      i.readyChat
        ? {
            label: `Share it with ${team}`,
            sub: `“${i.readyChat.title}” is ready to share.`,
            action: {
              label: "Share it",
              tone: "primary",
              kind: "share-ready-chat",
              busy: i.busy?.shareReadyChat,
            },
          }
        : {
            label: `Share it with ${team}`,
            sub:
              i.myPrivateChatCount > 0
                ? `Switch any chat below from Only you to ${team}.`
                : "Available after step 1",
          },
    ],
    canKeepPersonal: false,
  };
}

export const primaryButtonClass =
  "inline-flex h-7 shrink-0 items-center gap-1.5 rounded-full bg-[var(--color-primary)] px-3 text-[0.75rem] font-medium text-[var(--color-on-primary)] transition hover:bg-[var(--color-primary-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60";

const secondaryButtonClass =
  "inline-flex h-7 shrink-0 items-center rounded-full border border-[var(--color-border-strong)] px-3 text-[0.75rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60";

export function ProjectGettingStarted({
  model,
  onAction,
  onDismiss,
  onKeepPersonal,
}: {
  model: GettingStartedModel;
  onAction: (kind: StepActionKind) => void;
  onDismiss: () => void;
  onKeepPersonal: () => void;
}) {
  const done = model.steps.filter((s) => s.done).length;
  return (
    <section
      aria-label="Getting started"
      data-testid="getting-started"
      data-variant={model.variant}
      className="mb-4 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-4"
    >
      <div className="mb-1 flex items-center gap-2">
        <span className="font-[family-name:var(--font-code)] text-[0.68rem] font-semibold tracking-[0.08em] text-[var(--color-accent)]">
          GET STARTED
        </span>
        <span className="font-[family-name:var(--font-code)] text-[0.68rem] text-[var(--color-text-muted)]">
          {done} of {model.steps.length}
        </span>
        <span className="flex-1" />
        <button
          type="button"
          aria-label="Dismiss getting started"
          title="Hide for now"
          className="inline-flex size-7 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={onDismiss}
        >
          <Icon name="close" className="size-3.5" />
        </button>
      </div>
      <h2 className="m-0 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        {model.title}
      </h2>
      <p className="m-0 mt-0.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
        {model.lead}
      </p>
      <ol className="m-0 mt-3 grid list-none gap-0 p-0">
        {model.steps.map((s, idx) => (
          <li
            key={s.label}
            data-done={s.done ? "true" : "false"}
            className="flex items-center gap-3 border-t border-[var(--color-border-subtle)] py-2.5 first:border-t-0"
          >
            {s.done ? (
              <span
                role="img"
                aria-label="Done"
                className="grid size-5 shrink-0 place-items-center rounded-full bg-[color-mix(in_srgb,var(--color-success-strong)_18%,transparent)] text-[var(--color-success-soft)]"
              >
                <Icon name="check" className="size-3" />
              </span>
            ) : (
              <span className="grid size-5 shrink-0 place-items-center rounded-full border border-[var(--color-border-strong)] font-[family-name:var(--font-code)] text-[0.65rem] text-[var(--color-text-secondary)]">
                {idx + 1}
              </span>
            )}
            <span className="min-w-0 flex-1">
              <span
                className={[
                  "block text-[0.85rem]",
                  s.done
                    ? "text-[var(--color-text-muted)]"
                    : "text-[var(--color-text-primary)]",
                ].join(" ")}
              >
                {s.label}
              </span>
              {s.sub ? (
                <span className="block text-[0.74rem] leading-[1.45] text-[var(--color-text-muted)]">
                  {s.sub}
                </span>
              ) : null}
            </span>
            {s.action ? (
              <button
                type="button"
                disabled={s.action.busy}
                className={
                  s.action.tone === "primary"
                    ? primaryButtonClass
                    : secondaryButtonClass
                }
                onClick={() => s.action && onAction(s.action.kind)}
              >
                {s.action.label}
              </button>
            ) : null}
          </li>
        ))}
      </ol>
      {model.canKeepPersonal ? (
        <div className="mt-1 flex justify-end">
          <button
            type="button"
            className="rounded-[var(--radius-md)] px-2 py-1 text-[0.75rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={onKeepPersonal}
          >
            Keep personal
          </button>
        </div>
      ) : null}
    </section>
  );
}
