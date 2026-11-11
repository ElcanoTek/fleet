import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { Icon } from "./Icon";
import { CloseButton } from "@/app/shared/ui/CloseButton";

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

const INSTRUCTIONS_SUB = tr("Optional. Every chat here follows them.");

export function buildGettingStarted(i: GettingStartedInput): GettingStartedModel {
  const addInstructions: GettingStartedStep = {
    label: tr("Add instructions"),
    done: i.hasInstructions,
    sub: i.hasInstructions ? undefined : INSTRUCTIONS_SUB,
    action: i.hasInstructions
      ? undefined
      : { label: tr("Add"), tone: "secondary", kind: "add-instructions" },
  };

  // B25: no team, so there is nobody to share with — no share step at all.
  if (!i.myTeam && !i.projectTeam) {
    return {
      variant: "no-team",
      title: tr("Get started in {0}", [i.projectName]),
      lead: tr("Sharing needs a team. Admins: add yourself in Settings → Admin → Users, or create a team in Settings → Team. Everyone else: ask an admin."),
      steps: [
        addInstructions,
        {
          label: tr("Start a chat"),
          done: i.myChatCount > 0,
          action:
            i.myChatCount > 0
              ? undefined
              : { label: tr("New chat"), tone: "primary", kind: "new-chat" },
        },
      ],
      canKeepPersonal: false,
    };
  }

  const team = i.projectTeam || i.myTeam;
  const lead = tr("Chats start as Only you. Share the ones {0} should read and build on.", [team]);

  // B12: the owner's personal project — share the project first.
  if (!i.projectTeam) {
    return {
      variant: "owner-personal",
      title: tr("Work on this with {0}", [team]),
      lead: tr("Share the project first. Each chat stays Only you until you share it."),
      steps: [
        {
          label: tr("Share {0} with {1}", [i.projectName, team]),
          sub: tr("{0} sees the instructions and Team learnings. Chats stay Only you.", [team]),
          action: {
            label: tr("Share project"),
            tone: "primary",
            kind: "share-project",
            busy: i.busy?.shareProject,
          },
        },
        addInstructions,
        { label: tr("Share a chat with {0}", [team]), sub: tr("Available after step 1") },
      ],
      canKeepPersonal: true,
    };
  }

  // B6: the owner of a shared project.
  if (i.isOwner) {
    let share: GettingStartedStep;
    if (i.readyChat) {
      share = {
        label: tr("Share a chat with {0}", [team]),
        sub: tr("“{0}” is ready to share.", [i.readyChat.title]),
        action: {
          label: tr("Share it"),
          tone: "primary",
          kind: "share-ready-chat",
          busy: i.busy?.shareReadyChat,
        },
      };
    } else if (i.myPrivateChatCount > 0) {
      share = {
        label: tr("Share a chat with {0}", [team]),
        sub: tr("Switch any chat below from Only you to {0}.", [team]),
      };
    } else {
      share = {
        label: tr("Share a chat with {0}", [team]),
        sub: tr("Start one, then switch it to {0}.", [team]),
        action: { label: tr("New chat"), tone: "primary", kind: "new-chat" },
      };
    }
    return {
      variant: "owner-shared",
      title: tr("Get {0} working here", [team]),
      lead,
      steps: [{ label: tr("Project shared with {0}", [team]), done: true }, addInstructions, share],
      canKeepPersonal: false,
    };
  }

  // B8: a member of a shared project.
  const started = i.myChatCount > 0;
  return {
    variant: "member-shared",
    title: tr("Get started in {0}", [i.projectName]),
    lead,
    steps: [
      {
        label: tr("Start a chat here, or branch one"),
        done: started,
        sub: started ? undefined : tr("You can also move a chat in from the sidebar."),
        action: started
          ? undefined
          : { label: tr("New chat"), tone: "primary", kind: "new-chat" },
      },
      i.readyChat
        ? {
            label: tr("Share it with {0}", [team]),
            sub: tr("“{0}” is ready to share.", [i.readyChat.title]),
            action: {
              label: tr("Share it"),
              tone: "primary",
              kind: "share-ready-chat",
              busy: i.busy?.shareReadyChat,
            },
          }
        : {
            label: tr("Share it with {0}", [team]),
            sub:
              i.myPrivateChatCount > 0
                ? tr("Switch any chat below from Only you to {0}.", [team])
                : tr("Available after step 1"),
          },
    ],
    canKeepPersonal: false,
  };
}

export const primaryButtonClass =
  "inline-flex h-7 shrink-0 items-center gap-1.5 rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60";

const secondaryButtonClass =
  "inline-flex h-7 shrink-0 items-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 text-[0.75rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60";

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
      aria-label={tr("Getting started")}
      data-testid="getting-started"
      data-variant={model.variant}
      className="mb-4 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-4"
    >
      <div className="mb-1 flex items-center gap-2">
        <span className="font-[family-name:var(--font-code)] text-[0.68rem] font-semibold tracking-[0.08em] text-[var(--color-accent)]">
          {tr("GET STARTED")}
        </span>
        <span className="font-[family-name:var(--font-code)] text-[0.68rem] text-[var(--color-text-muted)]">
        {rich("{0} of {1}", done, model.steps.length)}
      </span>
        <span className="flex-1" />
        <CloseButton label={tr("Dismiss getting started")} onClick={onDismiss} />
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
                aria-label={tr("Done")}
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
            {tr("Keep personal")}
          </button>
        </div>
      ) : null}
    </section>
  );
}
