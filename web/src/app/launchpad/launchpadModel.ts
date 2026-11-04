// Patrol Launchpad — the pure half (docs/PATROL-LAUNCHPAD.md).
//
// Everything the wizard decides without touching the network lives here, so
// it can be tested on its own: the catalogue of things a patrol can watch, how
// a goal is matched to the connectors a person actually has, the cadence
// presets, and the template draft of the patrol's instructions the wizard
// shows before (or instead of) asking the assistant to write them.

export type LaunchpadConnector = {
  name: string;
  display_name?: string;
  description?: string;
  category?: string;
  always_on?: boolean;
  enabled?: boolean;
  remote?: boolean;
  status?: "available" | "needs_setup" | "unavailable";
};

export type PatrolMode = "report" | "suggest" | "act";

export type PatrolGoal = {
  id: string;
  title: string;
  blurb: string;
  icon: string;
  /** Words that, found in a connector's name, label, description or category, mean it can serve this goal. */
  keywords: string[];
  /** What kind of connector the goal needs, in words, for when none matches. */
  needs: string;
  /** The checklist line this goal contributes to the template draft. */
  step: string;
};

// The goals are the checks people in ad-tech operations ask for most, plus a
// few general ones, so a person with an inbox and nothing else still finds a
// useful first patrol. Matching is a hint, never a gate: any goal can be
// picked, and one with no matching connector says what it would need.
export const PATROL_GOALS: PatrolGoal[] = [
  {
    id: "deals",
    title: "Catch misconfigured deals",
    blurb: "Spot deals with wrong floors, targeting, dates or seats before they cost you.",
    icon: "target",
    keywords: ["deal", "ssp", "dsp", "pmp", "curat", "inventory", "trading", "programmatic"],
    needs: "a deal or SSP/DSP connector",
    step:
      "Check every active deal for misconfiguration — floor price, targeting, flight dates, seat or buyer IDs, and deals that are live but not bidding — and say exactly what looks wrong and how to fix it.",
  },
  {
    id: "spend",
    title: "Alert when spend drops off",
    blurb: "Notice pacing drops, stalled campaigns and budgets about to run out.",
    icon: "dollar-sign",
    keywords: ["spend", "pacing", "budget", "campaign", "dsp", "ads", "advert", "media", "deal", "analytics", "report"],
    needs: "a campaign, DSP or reporting connector",
    step:
      "Compare spend and pacing with the previous run and the same time yesterday; flag anything down more than 25% (a starting threshold the person can change), campaigns that stopped delivering, and budgets on track to run out early.",
  },
  {
    id: "reports",
    title: "Make sure reports get delivered",
    blurb: "Confirm the scheduled reports and files you depend on actually arrived.",
    icon: "file-text",
    keywords: ["report", "mail", "email", "inbox", "gmail", "outlook", "drive", "sharepoint", "sheet", "storage", "s3", "file"],
    needs: "an email, file or reporting connector",
    step:
      "Confirm the recurring reports and data files the person depends on arrived on time and look complete; list anything missing, late or empty, with when it was last seen.",
  },
  {
    id: "inbox",
    title: "Triage my inbox",
    blurb: "Surface what needs a reply, what is urgent, and what can wait.",
    icon: "mail",
    keywords: ["mail", "email", "inbox", "gmail", "outlook", "microsoft", "365", "message", "slack", "teams"],
    needs: "an email or messaging connector",
    step:
      "Scan new email and messages since the last run; pull out what needs a reply or a decision, anything urgent, and requests from clients or partners, each with a one-line summary and a suggested next step.",
  },
  {
    id: "optimize",
    title: "Suggest optimizations",
    blurb: "Find the bids, floors and budgets worth adjusting, with the reasoning.",
    icon: "zap",
    keywords: ["campaign", "dsp", "ssp", "deal", "bid", "analytics", "performance", "ads", "advert", "report"],
    needs: "a campaign, deal or analytics connector",
    step:
      "Look for the two or three optimizations most worth making now — bids, floors, budgets, targeting or underperforming line items — each with the evidence, the expected impact and the exact change to make.",
  },
  {
    id: "day",
    title: "Prep me for my day",
    blurb: "A short brief on meetings, deadlines and what changed overnight.",
    icon: "calendar",
    keywords: ["calendar", "meeting", "outlook", "gmail", "google", "microsoft", "365", "task", "project", "jira", "linear", "asana"],
    needs: "a calendar, email or task connector",
    step:
      "Brief the person on the day ahead: meetings and who they are with, deadlines coming due, and anything that changed since the last run that they should know before they start.",
  },
  {
    id: "anomalies",
    title: "Watch my data for anomalies",
    blurb: "Flag sudden jumps, gaps or broken feeds in the numbers you track.",
    icon: "activity",
    keywords: ["data", "database", "warehouse", "bigquery", "snowflake", "postgres", "sql", "analytics", "metric", "dashboard"],
    needs: "a database, warehouse or analytics connector",
    step:
      "Check the key metrics the person tracks for sudden jumps, drops, gaps or stale data compared with recent history, and say which look like real changes and which look like a broken feed.",
  },
];

/** connectorLabel names a connector the way the Tools picker does. */
export function connectorLabel(c: LaunchpadConnector): string {
  return (c.display_name || "").trim() || c.name;
}

/** usable is a connector the patrol can actually call: set up and reachable. */
export function usable(c: LaunchpadConnector): boolean {
  return !c.status || c.status === "available";
}

function haystack(c: LaunchpadConnector): string {
  return [c.name, c.display_name, c.description, c.category].filter(Boolean).join(" ").toLowerCase();
}

/**
 * matchingConnectors returns the selected, usable connectors whose name,
 * label, description or category mentions one of the goal's keywords.
 */
export function matchingConnectors(goal: PatrolGoal, connectors: LaunchpadConnector[]): LaunchpadConnector[] {
  return connectors.filter((c) => {
    if (!usable(c)) return false;
    const hay = haystack(c);
    return goal.keywords.some((k) => hay.includes(k));
  });
}

/**
 * initialSelection is the connectors the wizard pre-ticks: every usable one
 * the person already has on by default (the same seed a new chat gets), plus
 * the always-on rows, which are on regardless.
 */
export function initialSelection(connectors: LaunchpadConnector[]): string[] {
  return connectors.filter((c) => usable(c) && (c.always_on || c.enabled)).map((c) => c.name);
}

/**
 * optionalSelection is what the patrol chat persists as its connector set:
 * the ticked, usable, optional rows. Always-on rows are status, never part of
 * a conversation's opt-in list (see chat/ui/mcpSelection.ts).
 */
export function optionalSelection(connectors: LaunchpadConnector[], selected: string[]): string[] {
  const picked = new Set(selected);
  return connectors.filter((c) => picked.has(c.name) && usable(c) && !c.always_on).map((c) => c.name);
}

export type CadencePreset = { id: string; cron: string; label: string; hint: string };

// Five-minute floor is the server's (patrol schedules); none of these is near it.
export const CADENCE_PRESETS: CadencePreset[] = [
  { id: "weekday-morning", cron: "0 8 * * 1-5", label: "Weekday mornings", hint: "8:00, Monday to Friday" },
  { id: "twice-daily", cron: "0 9,16 * * 1-5", label: "Twice a workday", hint: "9:00 and 16:00, Monday to Friday" },
  { id: "every-2h", cron: "0 */2 * * *", label: "Every 2 hours", hint: "Around the clock" },
  { id: "hourly", cron: "0 * * * *", label: "Every hour", hint: "Around the clock" },
  { id: "daily", cron: "0 8 * * *", label: "Every morning", hint: "8:00, every day" },
  { id: "weekly", cron: "0 9 * * 1", label: "Monday kickoff", hint: "9:00 every Monday" },
];

export const MODE_OPTIONS: { id: PatrolMode; title: string; blurb: string }[] = [
  {
    id: "report",
    title: "Just report",
    blurb: "Looks and tells you. Never changes anything.",
  },
  {
    id: "suggest",
    title: "Suggest & ask",
    blurb: "Recommends fixes and stages them as approval cards for you to press.",
  },
  {
    id: "act",
    title: "Act for me",
    blurb: "Handles routine, reversible fixes itself (auto-approve on) and lists everything it did.",
  },
];

/** Suggested non-negotiables: tap to add, edit before saving. */
export const MEMORY_SUGGESTIONS: string[] = [
  "Never send an email or message on my behalf without asking me first.",
  "Lead with the number and the action; keep updates short.",
  "Report money in USD with two decimals.",
  "Flag anything that could cost more than $500 as urgent.",
  "My working hours are 9:00–18:00 on weekdays.",
  "Never change a live deal or campaign without my approval.",
];

export type LaunchpadAnswers = {
  goals: string[];
  notes: string;
  connectors: LaunchpadConnector[];
  selected: string[];
  mode: PatrolMode;
  cadenceLabel: string;
};

/** goalById resolves picked goal ids, in catalogue order. */
export function pickedGoals(ids: string[]): PatrolGoal[] {
  const set = new Set(ids);
  return PATROL_GOALS.filter((g) => set.has(g.id));
}

/** draftTitle names the patrol from its goals ("Deals & spend patrol"). */
export function draftTitle(goalIds: string[]): string {
  const short: Record<string, string> = {
    deals: "Deals",
    spend: "Spend",
    reports: "Reports",
    inbox: "Inbox",
    optimize: "Optimization",
    day: "Daily brief",
    anomalies: "Data",
  };
  const names = pickedGoals(goalIds).map((g) => short[g.id] ?? g.title);
  if (names.length === 0) return "My patrol";
  if (names.length === 1) return `${names[0]} patrol`;
  if (names.length === 2) return `${names[0]} & ${names[1].toLowerCase()} patrol`;
  return `${names[0]}, ${names[1].toLowerCase()} & more patrol`;
}

const MODE_RULES: Record<PatrolMode, string> = {
  report:
    "Observe and report only. Never change anything, send anything or stage an action — recommend, and let me decide.",
  suggest:
    "Recommend specific fixes and stage each one for my approval. Do not make changes yourself.",
  act:
    "You may take routine, reversible actions yourself; list every action you took in the report. Ask me first for anything costly, irreversible or outside these checks.",
};

/**
 * draftPrompt is the template draft of the patrol's standing instructions —
 * what the assistant reads every time the patrol wakes. It is useful as-is,
 * and it is what the wizard keeps if "Draft it with AI" is unavailable.
 */
export function draftPrompt(a: LaunchpadAnswers): string {
  const goals = pickedGoals(a.goals);
  const selected = new Set(a.selected);
  const active = a.connectors.filter((c) => selected.has(c.name) && usable(c));
  const lines: string[] = [];
  lines.push("**Mission**");
  lines.push(
    "You are on patrol for me. Each time this runs, check the things below with my connected tools and tell me what needs my attention — and what to do about it.",
  );
  lines.push("");
  lines.push("**Each run**");
  let n = 0;
  for (const g of goals) {
    n += 1;
    const using = matchingConnectors(g, active).map(connectorLabel);
    const via = using.length > 0 ? ` (use ${joinWords(using)})` : ` (if no connector can do this, tell me once that it needs ${g.needs})`;
    lines.push(`${n}. ${g.step}${via}`);
  }
  const notes = a.notes.trim();
  if (notes) {
    n += 1;
    lines.push(`${n}. ${notes}`);
  }
  lines.push("");
  lines.push("**Report**");
  lines.push(
    "Start with one headline: “✅ All clear” or “⚠️ N items need you”. Then list the items by urgency — what you found, why it matters, and the next step. Compare with your previous report in this chat: say what is new, what is resolved and what is still open, instead of repeating unchanged findings.",
  );
  lines.push("");
  lines.push("**Boundaries**");
  lines.push(MODE_RULES[a.mode]);
  if (active.length > 0) {
    lines.push(`Tools switched on for this patrol: ${joinWords(active.map(connectorLabel))}.`);
  }
  if (a.cadenceLabel.trim()) {
    lines.push(`Schedule: ${a.cadenceLabel.trim()}.`);
  }
  return lines.join("\n");
}

function joinWords(items: string[]): string {
  if (items.length <= 1) return items.join("");
  if (items.length === 2) return `${items[0]} and ${items[1]}`;
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}
