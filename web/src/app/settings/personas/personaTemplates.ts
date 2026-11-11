import { t as tr } from "@/app/lib/i18n";
import {
  describeCronExpression,
  describeCronExpressionShort,
} from "@/app/shared/lib/cron";

// Starting points and small helpers for Settings → Personas
// (docs/USER-PERSONAS.md). Kept apart from the page so they are unit-testable
// and the page stays about layout.

export type PersonaKind = "assistant" | "twin";
export type PersonaAutonomy = "advise" | "act";

export type PersonaPatrol = {
  title: string;
  cron: string;
  timezone?: string;
  prompt: string;
};

export type PersonaDraft = {
  id: string; // "" = new
  name: string;
  display_name: string;
  description: string;
  instructions: string;
  base_persona: string;
  kind: PersonaKind;
  autonomy: PersonaAutonomy;
  patrols: PersonaPatrol[];
};

export const EMPTY_PERSONA: PersonaDraft = {
  id: "",
  name: "",
  display_name: "",
  description: "",
  instructions: "",
  base_persona: "",
  kind: "assistant",
  autonomy: "advise",
  patrols: [],
};

// Cadence presets offered for a suggested patrol. "custom" keeps whatever
// cron the user typed; the server enforces the same ≥5-minute floor a started
// patrol has.
export const CADENCE_PRESETS: { cron: string; label: string }[] = [
  { cron: "0 * * * *", label: tr("Every hour") },
  { cron: "0 */2 * * *", label: tr("Every 2 hours") },
  { cron: "0 */4 * * *", label: tr("Every 4 hours") },
  { cron: "0 8 * * 1-5", label: tr("Weekdays at 8:00") },
  { cron: "0 9 * * 1-5", label: tr("Weekdays at 9:00") },
  { cron: "0 9,13,17 * * 1-5", label: tr("Weekdays at 9, 13 and 17") },
  { cron: "0 8 * * *", label: tr("Daily at 8:00") },
  { cron: "0 9 * * 1", label: tr("Mondays at 9:00") },
];

/** cadenceLabel names a cron in words when it is a preset or describable schedule, else shows it. */
export function cadenceLabel(cron: string): string {
  const trimmed = cron.trim();
  const preset = CADENCE_PRESETS.find((p) => p.cron === trimmed);
  if (preset) return preset.label;
  return (
    describeCronExpressionShort(trimmed) ||
    describeCronExpression(trimmed) ||
    trimmed
  );
}

/**
 * slugify derives the persona name (lowercase kebab, ≤48 chars — the server's
 * shape) from a display name, so most people never type the slug.
 */
export function slugify(displayName: string): string {
  return displayName
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48)
    .replace(/-+$/g, "");
}

/** The viewer's IANA zone, for new patrol rows (UTC when unavailable). */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

export type PersonaTemplate = {
  id: string;
  label: string;
  hint: string;
  draft: () => Partial<PersonaDraft>;
};

export const PERSONA_TEMPLATES: PersonaTemplate[] = [
  {
    id: "twin",
    label: tr("Digital twin"),
    hint: tr("Thinks, prioritizes and writes like you"),
    draft: () => ({
      display_name: tr("My Twin"),
      kind: "twin",
      autonomy: "advise",
      description: tr("Thinks and writes the way I do; watches my work while I'm away."),
      instructions: [
        tr("## Who I am"),
        tr("- Role: …"),
        tr("- What I own and care about most: …"),
        "",
        tr("## How I decide"),
        tr("- My rules of thumb and thresholds (e.g. flag anything under 80% of target): …"),
        tr("- What I escalate immediately vs. what can wait for my morning summary: …"),
        "",
        tr("## How I write"),
        tr("- Lead with the answer and the number; short bullet points; no filler."),
        "",
        tr("## Never"),
        tr("- Commit me to anything, contact a client, or change a live setting without asking."),
      ].join("\n"),
      patrols: [
        {
          title: tr("Morning briefing"),
          cron: "0 8 * * 1-5",
          timezone: browserTimeZone(),
          prompt:
            tr("Prepare my morning briefing: what changed since yesterday in the work I own, anything that needs my decision today, and your recommended next steps."),
        },
      ],
    }),
  },
  {
    id: "patrol",
    label: tr("Patrol analyst"),
    hint: tr("Watches numbers on a schedule and flags anomalies"),
    draft: () => ({
      display_name: tr("Patrol Analyst"),
      kind: "assistant",
      autonomy: "advise",
      description: tr("Checks my key numbers on a schedule and flags what moved."),
      instructions: [
        tr("You watch the metrics I name and report only what changed or crossed a threshold."),
        "",
        tr("- Compare against the previous run in this chat; say what moved and by how much."),
        tr("- Rank findings by impact. If nothing crossed a threshold, say so in one line."),
        tr("- End each report with recommended actions, numbered, for me to approve."),
      ].join("\n"),
      patrols: [
        {
          title: tr("Every-4-hours check"),
          cron: "0 */4 * * *",
          timezone: browserTimeZone(),
          prompt:
            tr("Check the metrics we track in this chat, compare with the last check, and report anything that crossed a threshold with a recommended action."),
        },
      ],
    }),
  },
  {
    id: "writer",
    label: tr("Writing partner"),
    hint: tr("Drafts in your voice for your review"),
    draft: () => ({
      display_name: tr("Writing Partner"),
      kind: "assistant",
      autonomy: "advise",
      description: tr("Drafts emails and updates in my voice for me to review."),
      instructions: [
        tr("Draft in my voice: warm, direct, short sentences, no jargon."),
        "",
        tr("- Offer one draft, then a shorter alternative."),
        tr("- Never send anything — I review and send myself."),
      ].join("\n"),
      patrols: [],
    }),
  },
];

/**
 * twinBuilderMessage composes the chat message the digital-twin builder puts
 * in the composer. It asks the assistant to use persona_studio and leaves the
 * user to press Send — nothing runs until they do.
 */
export function twinBuilderMessage(opts: {
  about: string;
  useHistory: boolean;
  autonomy: PersonaAutonomy;
}): string {
  const about = opts.about.trim();
  const steps = [
    opts.useHistory
      ? tr("Read what I've written in my recent chats (persona_studio, action \"history\") to learn my priorities, vocabulary and the work I repeat.")
      : tr("Don't read my chat history — work from what I tell you and my saved memories."),
    tr("Ask me up to 3 short questions if something important is unclear (my role, my thresholds, what I never want done without asking)."),
    tr("Propose a digital-twin persona (kind \"twin\", autonomy \"{0}\") with persona_studio, action \"propose\": concrete instructions for how I think, decide and write, plus suggested patrols for the checks I repeat, with a cadence and clear instructions for each.", [opts.autonomy]),
    tr("Then tell me what you proposed and where to approve it."),
  ];
  return [
    tr("Help me build my digital twin — an assistant that thinks and works the way I do and can run patrols for me."),
    "",
    tr("About me: {0}", [about || tr("(ask me)")]),
    "",
    ...steps.map((s, i) => `${i + 1}. ${s}`),
  ].join("\n");
}
