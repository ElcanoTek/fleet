// Patrol Launchpad (docs/PATROL-LAUNCHPAD.md) — the guided path from "nothing
// set up" to "an assistant on patrol, reporting back on a schedule".
//
// Six steps: what a patrol is → hook up tools → ground rules (who patrols,
// and the non-negotiables saved as memories) → what to watch and how
// hands-on → how often → review and launch. Launch goes through the same
// endpoints Settings → Personas' "Start patrol" uses — create the chat, set
// its connectors, arm the schedule, run the first check — so every rule about
// patrols (the five-minute floor, auto-approve, ownership) stays the server's.
//
// Progress lives in sessionStorage so a person can leave to connect a tool
// (that opens in a new tab, but a reload or a closed dialog must not cost them
// their answers) and come back to the same step.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getBootstrap } from "@/app/lib/bootstrap";
import { navigate } from "@/app/lib/navigation";
import { currentDefaultModel } from "@/app/lib/modelAliases";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { Icon } from "@/app/shared/ui/Icon";
import { PatrolSchedulePicker } from "@/app/shared/ui/PatrolSchedulePicker";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { browserTimeZone } from "@/app/shared/lib/timezones";
import { btnClass, SETTINGS_INPUT } from "@/app/settings/ui/atoms";
import { MindArt, type MindLevel } from "./MindArt";
import {
  CADENCE_PRESETS,
  connectorLabel,
  draftPrompt,
  draftTitle,
  initialSelection,
  matchingConnectors,
  MEMORY_SUGGESTIONS,
  MODE_OPTIONS,
  optionalSelection,
  PATROL_GOALS,
  usable,
  type LaunchpadConnector,
  type PatrolMode,
} from "./launchpadModel";
import { responseErrorText } from "@/app/lib/responseError";

export const LAUNCHPAD_PROGRESS_ITEM = "fleet.launchpad.v1";

// progressItem scopes saved answers to the signed-in account, so on a shared
// browser (sign out, sign in as someone else in the same tab — sessionStorage
// survives that) the next person never sees or launches the last one's notes,
// persona or drafted instructions.
// recoveryItem is where an unsettled launch is kept (see setCreated).
export function recoveryItem(): string {
  return `${progressItem()}:recovery`;
}

export function progressItem(): string {
  const email = getBootstrap().session?.email?.trim().toLowerCase() ?? "";
  return `${LAUNCHPAD_PROGRESS_ITEM}:${email}`;
}
export const LAUNCHPAD_LAUNCHED_ITEM = "fleet.launchpad.launched";

// "why" is the preamble (the expanding-mind ladder: why a patrol is the big
// step), "welcome" the run-through of what the wizard is about to do.
const STEPS = [
  { id: "why", label: "Why", icon: "sparkles" },
  { id: "welcome", label: "Plan", icon: "map-pin" },
  { id: "tools", label: "Tools", icon: "plug" },
  { id: "rules", label: "Rules", icon: "brain" },
  { id: "watch", label: "Watch", icon: "target" },
  { id: "schedule", label: "Rhythm", icon: "clock" },
  { id: "launch", label: "Launch", icon: "zap" },
] as const;
type StepId = (typeof STEPS)[number]["id"];

type Memory = {
  id: string;
  content: string;
  kind?: string;
  pinned?: boolean;
  source?: string;
  retired_at?: number;
};

// activeMemories is what a turn actually reads: /api/memories is the
// management view and also lists retired memories and unaccepted proposals
// (source "proposed"), neither of which is injected into a chat — so neither
// is shown as a ground rule or handed to the patrol drafter.
// newRequestId makes a launch's idempotency key: random and unguessable where
// the browser offers crypto.randomUUID (every secure context), with a
// time-and-random fallback that is still unique per launch.
function newRequestId(): string {
  try {
    if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return `launchpad-${crypto.randomUUID()}`;
  } catch {
    // fall through
  }
  return `launchpad-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export function activeMemories(all: Memory[]): Memory[] {
  return all.filter((m) => !m.retired_at && m.source !== "proposed");
}

/**
 * withMemory adds a just-saved memory to the roster in the store's order —
 * pinned first — so the drafter's 50-memory window (taken from the front)
 * keeps a new pinned rule instead of dropping it off the end.
 */
export function withMemory(list: Memory[], mem: Memory): Memory[] {
  if (list.some((m) => m.id === mem.id)) return list;
  const at = mem.pinned ? 0 : list.filter((m) => m.pinned).length;
  return [...list.slice(0, at), mem, ...list.slice(at)];
}
type UserPersona = { ref: string; display_name: string; description?: string };

type Saved = {
  step: StepId;
  selected: string[] | null;
  persona: string;
  goals: string[];
  notes: string;
  mode: PatrolMode;
  cron: string;
  timezone: string;
  title: string;
  prompt: string;
  // Edits are tracked per field: renaming the patrol must not freeze its
  // instructions. promptBasis is the template the custom instructions were
  // written against, so a later change to the plan can be pointed out.
  titleEdited: boolean;
  promptEdited: boolean;
  promptBasis: string;
  summary: string;
  // Non-negotiables saved on Ground rules: they are memories the moment they
  // are saved, which the launch step must not pretend otherwise about.
  memoriesAdded: number;
};

const FRESH: Saved = {
  step: "why",
  selected: null,
  persona: "",
  goals: [],
  notes: "",
  mode: "suggest",
  cron: CADENCE_PRESETS[0].cron,
  timezone: "",
  title: "",
  prompt: "",
  titleEdited: false,
  promptEdited: false,
  promptBasis: "",
  summary: "",
  memoriesAdded: 0,
};

function loadSaved(): Saved {
  try {
    const raw = window.sessionStorage.getItem(progressItem());
    if (!raw) return { ...FRESH, timezone: browserTimeZone() };
    const parsed = JSON.parse(raw) as Partial<Saved>;
    const step = STEPS.some((s) => s.id === parsed.step) ? (parsed.step as StepId) : "why";
    return { ...FRESH, timezone: browserTimeZone(), ...parsed, step };
  } catch {
    return { ...FRESH, timezone: browserTimeZone() };
  }
}

// A failed step names itself ahead of the server's reason ("Could not arm the
// patrol: over the cap"), so the banner says which of the launch's several
// requests refused. An empty body already reads "<step> (NNN)" from
// responseErrorText and is not prefixed twice.
async function stepErrorText(res: Response, step: string): Promise<string> {
  const text = await responseErrorText(res, step);
  return text.startsWith(`${step} (`) ? text : `${step}: ${text}`;
}

type LaunchPhase = "conversation" | "tools" | "schedule" | "first-run";
type LaunchState = {
  phase: LaunchPhase | "done";
  error?: string;
  conversationId?: string;
  // How the first check went once the patrol was armed.
  // "unknown": the run-now request got no answer at all, so it may or may
  // not have started — the copy must not invite a second, duplicate run.
  firstRun?: "running" | "refused" | "skipped" | "unknown";
};
const LAUNCH_PHASES: { id: LaunchPhase; label: string }[] = [
  { id: "conversation", label: "Opening the patrol chat" },
  { id: "tools", label: "Switching on your tools" },
  { id: "schedule", label: "Arming the schedule" },
  { id: "first-run", label: "Starting the first check" },
];

export function PatrolLaunchpad({ onClose }: { onClose: () => void }) {
  const [state, setState] = useState<Saved>(loadSaved);
  const patch = useCallback((p: Partial<Saved>) => setState((s) => ({ ...s, ...p })), []);
  const stepIndex = STEPS.findIndex((s) => s.id === state.step);

  // Data the steps read.
  const [connectors, setConnectors] = useState<LaunchpadConnector[] | null>(null);
  const [memories, setMemories] = useState<Memory[] | null>(null);
  const [workspacePersonas, setWorkspacePersonas] = useState<string[]>([]);
  const [defaultPersona, setDefaultPersona] = useState("");
  const [userPersonas, setUserPersonas] = useState<UserPersona[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Whether the catalog ever loaded. Until it has, the person's selection is
  // unknown, and launch must leave the server's default connector seed alone
  // rather than replace it with an empty set.
  const [catalogLoaded, setCatalogLoaded] = useState(false);
  // A read-only viewer can read the preamble but cannot save memories or
  // create a patrol (every write is refused for the viewer role), so the
  // flow stops there with an explanation instead of failing at Launch.
  const [readOnly, setReadOnly] = useState(false);

  // Each read takes a generation; only the latest one's answer lands, so a
  // slow earlier read (mount, then a focus refresh) cannot overwrite a newer.
  const connectorsGen = useRef(0);
  const loadConnectors = useCallback(async () => {
    const gen = ++connectorsGen.current;
    try {
      const res = await fetch("/api/mcp-servers", { cache: "no-store" });
      if (!res.ok) throw new Error(await stepErrorText(res, "Could not load your connectors"));
      const data = (await res.json()) as { servers?: LaunchpadConnector[] };
      if (gen !== connectorsGen.current) return;
      setConnectors(data.servers ?? []);
      setCatalogLoaded(true);
      setLoadError(null);
    } catch (e) {
      if (gen !== connectorsGen.current) return;
      setConnectors((cur) => cur ?? []);
      // A failed refresh leaves the catalog unknown again: the list on screen
      // may be stale (a tool connected or removed in the other tab), so launch
      // must not replace the server's connector seed with it.
      setCatalogLoaded(false);
      setLoadError(e instanceof Error ? e.message : "Could not load your connectors.");
    }
  }, []);

  const [memoriesFailed, setMemoriesFailed] = useState(false);
  // A local add bumps the generation too: a read that left before the save
  // must not land after it and drop the memory the person just added.
  const memoriesGen = useRef(0);
  const loadMemories = useCallback(async () => {
    const gen = ++memoriesGen.current;
    try {
      const res = await fetch("/api/memories", { cache: "no-store" });
      if (!res.ok) throw new Error();
      const data = (await res.json()) as { memories?: Memory[] };
      if (gen !== memoriesGen.current) return;
      setMemories(activeMemories(data.memories ?? []));
      setMemoriesFailed(false);
    } catch {
      if (gen !== memoriesGen.current) return;
      // Unknown is not "none": a failed read keeps the roster unknown, so the
      // Plan step does not claim zero memories and the AI draft — which must
      // not contradict memories every patrol turn still receives — waits.
      setMemoriesFailed(true);
    }
  }, []);

  useEffect(() => {
    // Deferred a microtask so no state is set synchronously inside the effect.
    queueMicrotask(() => {
      void loadConnectors();
      void loadMemories();
    });
    void (async () => {
      try {
        const res = await fetch("/api/me", { cache: "no-store" });
        if (!res.ok) return;
        const me = (await res.json()) as { role?: string };
        if (me.role === "viewer") {
          setReadOnly(true);
          // A viewer sees only the preamble, even with progress saved on a
          // later step from before their role changed (or after a quick Next
          // while this read was in flight). The server still refuses every
          // write a viewer attempts; this keeps the screen honest.
          setState((cur) => (cur.step === "why" ? cur : { ...cur, step: "why" }));
        }
      } catch {
        // Unknown role: the server still refuses what a viewer cannot do.
      }
    })();
    void (async () => {
      try {
        const res = await fetch("/api/personas", { cache: "no-store" });
        if (!res.ok) return;
        const data = (await res.json()) as {
          personas?: string[];
          default?: string;
          user_personas?: UserPersona[] | null;
        };
        const workspace = data.personas ?? [];
        // null means the personal roster could not be read ("unknown"), not
        // "none" — a saved user: persona is only judged against a real list.
        const usersKnown = Array.isArray(data.user_personas);
        const users = data.user_personas ?? [];
        setWorkspacePersonas(workspace);
        setDefaultPersona(data.default ?? "");
        setUserPersonas(users);
        // A persona saved in earlier progress may have been deleted since:
        // fall back to the workspace default rather than launch a stale ref.
        setState((cur) =>
          cur.persona &&
          !workspace.includes(cur.persona) &&
          (cur.persona.startsWith("user:") ? usersKnown && !users.some((u) => u.ref === cur.persona) : true)
            ? { ...cur, persona: "" }
            : cur,
        );
      } catch {
        // Personas are optional; the patrol runs on the workspace default.
      }
    })();
  }, [loadConnectors, loadMemories]);

  // Connecting a tool happens in a new tab; coming back refreshes the list.
  useEffect(() => {
    const onFocus = () => void loadConnectors();
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [loadConnectors]);

  // Persist progress (not while launching or after launch: those clear it).
  const [launch, setLaunch] = useState<LaunchState | null>(null);
  const [drafting, setDrafting] = useState(false);
  // What a launch has created so far, so a failure can be rolled back.
  // What a launch has created so far. `pendingCreate` holds the create
  // request while its outcome is unknown (the request or its reply was lost):
  // the chat may exist without its ID ever reaching us. The request carries a
  // per-launch idempotency key (client_request_id), so sending it again is
  // safe and answers with the chat it made, if it made one.
  type Created = {
    conversationId: string;
    scheduleId: string;
    pendingCreate?: { body: Record<string, string> };
  };
  const created = useRef<Created>({ conversationId: "", scheduleId: "" });
  // What a launch has created is also kept in session storage while it is
  // unsettled (a chat made, or a create whose outcome is unknown), so a
  // reload at the error screen can still settle it and roll it back instead
  // of forgetting it. Every change goes through setCreated.
  const setCreated = useCallback((next: Created) => {
    created.current = next;
    try {
      if (next.conversationId || next.pendingCreate) {
        window.sessionStorage.setItem(recoveryItem(), JSON.stringify(next));
      } else {
        window.sessionStorage.removeItem(recoveryItem());
      }
    } catch {
      // Storage unavailable: recovery lasts for this page only.
    }
  }, []);
  // A launch left unsettled by a reload comes back on its error screen, where
  // Back to the plan (or closing) settles and rolls it back.
  useEffect(() => {
    let saved: Created | null = null;
    try {
      const raw = window.sessionStorage.getItem(recoveryItem());
      saved = raw ? (JSON.parse(raw) as Created) : null;
    } catch {
      saved = null;
    }
    if (!saved || !(saved.conversationId || saved.pendingCreate)) return;
    const recovered = saved;
    created.current = recovered;
    queueMicrotask(() => {
      setState((cur) => ({ ...cur, step: "launch" }));
      setLaunch({
        phase: recovered.pendingCreate ? "conversation" : "tools",
        error: "Your last launch didn’t finish. Go back to the plan to clean up what it left, then launch again.",
        conversationId: recovered.conversationId || undefined,
      });
    });
  }, []);
  // Sends a keyed create and returns the chat's ID, or "" when the outcome is
  // unknown (no reply, or one that could not be read). A clean HTTP error
  // throws: the server refused, so nothing was made.
  const sendCreate = async (body: Record<string, string>): Promise<string> => {
    const res = await fetch("/api/conversations", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).catch(() => null);
    if (!res) return "";
    if (!res.ok) throw new Error(await stepErrorText(res, "Could not create the patrol chat"));
    return res
      .json()
      .then((d: { id?: string }) => (typeof d?.id === "string" ? d.id : ""))
      .catch(() => "");
  };
  // Settles an unknown create by sending it again with the same key: the
  // server answers with the chat the first attempt made (or makes it now, to
  // be rolled back like any other). True when settled.
  const resolvePendingCreate = async (): Promise<boolean> => {
    const pending = created.current.pendingCreate;
    if (!pending) return true;
    const id = await sendCreate(pending.body).catch(() => "");
    if (!id) return false;
    setCreated({ ...created.current, conversationId: id, pendingCreate: undefined });
    return true;
  };
  useEffect(() => {
    if (launch) return;
    try {
      window.sessionStorage.setItem(progressItem(), JSON.stringify(state));
    } catch {
      // Private mode: progress just is not remembered.
    }
  }, [state, launch]);

  // The person's optional choices, plus every usable always-on connector the
  // catalog has now — one set up in another tab after a box was ticked must
  // still count, since the patrol will use it.
  const selected = useMemo(() => {
    const base = state.selected ?? (connectors ? initialSelection(connectors) : []);
    const alwaysOn = (connectors ?? []).filter((c) => c.always_on && usable(c)).map((c) => c.name);
    return [...new Set([...base, ...alwaysOn])];
  }, [state.selected, connectors]);
  const cadenceText = useMemo(() => {
    const preset = CADENCE_PRESETS.find((p) => p.cron === state.cron.trim());
    const words = preset ? `${preset.label} (${preset.hint})` : describeCronExpression(state.cron) || state.cron;
    return `${words}, ${state.timezone || "UTC"}`;
  }, [state.cron, state.timezone]);

  const template = useMemo(
    () =>
      draftPrompt({
        goals: state.goals,
        notes: state.notes,
        connectors: connectors ?? [],
        selected,
        mode: state.mode,
        cadenceLabel: cadenceText,
      }),
    [state.goals, state.notes, connectors, selected, state.mode, cadenceText],
  );
  // Until the person edits the instructions (or the assistant drafts them),
  // the review step shows the live template, so going back to change a goal
  // updates the draft instead of leaving a stale one.
  const title = state.titleEdited ? state.title : draftTitle(state.goals);
  // The name the patrol is actually saved under: an emptied name field falls
  // back to "My patrol" in both requests and on every screen that names it.
  const savedTitle = title.trim() || "My patrol";
  const prompt = state.promptEdited ? state.prompt : template;
  const promptStale = state.promptEdited && state.promptBasis !== template;

  const canNext =
    state.step !== "watch" || state.goals.length > 0 || state.notes.trim().length > 0;
  const go = (delta: number) => {
    const next = STEPS[Math.min(STEPS.length - 1, Math.max(0, stepIndex + delta))];
    patch({ step: next.id });
    bodyRef.current?.scrollTo?.({ top: 0 });
  };
  const bodyRef = useRef<HTMLDivElement | null>(null);

  const doLaunch = async () => {
    // Never launch over a draft in flight: it would save the template and
    // then show the assistant's text as if it had been launched.
    if (drafting) return;
    let phase: LaunchPhase = "conversation";
    const setPhase = (next: LaunchPhase) => {
      phase = next;
      setLaunch({ phase: next });
    };
    setCreated({ conversationId: "", scheduleId: "" });
    try {
      setPhase("conversation");
      // The connector set to persist. With a known catalog it is what the
      // Tools step shows. When the catalog is unknown (a refresh failed) but
      // the person made explicit choices, read it again now, before anything
      // is created: skipping the replacement would silently drop a tool the
      // plan promised, and a stale list could drop one set up meanwhile. If
      // it still cannot be read the launch stops here, with nothing created.
      // Never chose and never loaded: the server's defaults stand.
      let toolsCatalog: LaunchpadConnector[] | null = catalogLoaded ? (connectors ?? []) : null;
      if (!toolsCatalog && state.selected !== null) {
        const res = await fetch("/api/mcp-servers", { cache: "no-store" }).catch(() => null);
        if (!res?.ok) throw new Error("Couldn’t confirm the tools you picked. Check your connection and try again.");
        toolsCatalog = ((await res.json()) as { servers?: LaunchpadConnector[] }).servers ?? [];
      }
      const toolsSelection = toolsCatalog
        ? [
            ...new Set([
              ...(state.selected ?? initialSelection(toolsCatalog)),
              ...toolsCatalog.filter((c) => c.always_on && usable(c)).map((c) => c.name),
            ]),
          ]
        : [];
      const body: Record<string, string> = { title: savedTitle, model: currentDefaultModel() };
      if (state.persona) body.persona = state.persona;
      // A fresh idempotency key per launch: a create whose reply is lost is
      // sent again with it (now, and on Back or close), and the server
      // answers with the chat it already made instead of a second one.
      body.client_request_id = newRequestId();
      let conversationId = await sendCreate(body);
      if (!conversationId) conversationId = await sendCreate(body); // one immediate retry
      if (!conversationId) {
        setCreated({ ...created.current, pendingCreate: { body } });
        throw new Error(
          `Couldn’t confirm whether the patrol chat “${body.title}” was created. Check your connection and try again.`,
        );
      }
      setCreated({ ...created.current, conversationId });
      const base = `/api/conversations/${encodeURIComponent(conversationId)}`;

      setPhase("tools");
      // Only replace the chat's connector set when the catalog is known: an
      // unknown selection keeps the defaults the server seeded at creation.
      if (toolsCatalog) {
        const toolsRes = await fetch(`${base}/mcp-servers`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled_optional: optionalSelection(toolsCatalog, toolsSelection), accounts: {} }),
        });
        if (!toolsRes.ok) throw new Error(await stepErrorText(toolsRes, "Could not switch on the patrol's tools"));
      }

      setPhase("schedule");
      const schedRes = await fetch(`${base}/schedules`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title: savedTitle,
          prompt: prompt.trim(),
          cron: state.cron.trim(),
          timezone: state.timezone || "UTC",
          auto_approve: state.mode === "act",
        }),
      });
      if (!schedRes.ok) throw new Error(await stepErrorText(schedRes, "Could not arm the patrol"));
      const sched = (await schedRes.json()) as { schedule?: { id: string } };
      const scheduleId = sched.schedule?.id ?? "";
      setCreated({ ...created.current, scheduleId });

      setPhase("first-run");
      // The patrol is armed from here on; a refused first run (a full queue,
      // say) is reported, not treated as a failed launch.
      let firstRun: LaunchState["firstRun"] = "skipped";
      if (scheduleId) {
        const runRes = await fetch(`${base}/schedules/${encodeURIComponent(scheduleId)}/run-now`, {
          method: "POST",
        }).catch(() => null);
        firstRun = runRes === null ? "unknown" : runRes.ok ? "running" : "refused";
      }
      try {
        window.sessionStorage.removeItem(progressItem());
        window.localStorage.setItem(LAUNCHPAD_LAUNCHED_ITEM, "1");
      } catch {
        // Storage unavailable: nothing to clean up.
      }
      setCreated({ conversationId: "", scheduleId: "" });
      setLaunch({ phase: "done", conversationId, firstRun });
    } catch (e) {
      setLaunch({
        phase,
        error: e instanceof Error ? e.message : "Launch failed.",
        conversationId: created.current.conversationId || undefined,
      });
    }
  };

  // rollback undoes a half-finished launch before going back to the plan, so
  // a retry never leaves an orphaned chat — or an armed patrol — behind. The
  // schedule is cancelled first: on a deployment that soft-deletes
  // conversations, deleting the chat alone would not remove it.
  // Each step counts as done when the server confirms it, or says the thing
  // is already gone (404). Anything else keeps what is left in `created` and
  // shows an error, so Back to the plan (or closing) tries again rather than
  // forgetting a chat — or an armed patrol — it could not remove.
  const backToPlan = async (): Promise<boolean> => {
    // An unknown create first: find the chat it may have made, or stay put.
    if (!(await resolvePendingCreate())) {
      setLaunch((cur) => ({
        ...(cur ?? { phase: "conversation" }),
        error: `Couldn’t confirm whether the patrol chat “${created.current.pendingCreate?.body.title ?? ""}” was created. Check your connection and try again.`,
      }));
      return false;
    }
    const { conversationId, scheduleId } = created.current;
    const settled = (res: Response | null) => res !== null && (res.ok || res.status === 404);
    if (conversationId) {
      const base = `/api/conversations/${encodeURIComponent(conversationId)}`;
      if (scheduleId) {
        const res = await fetch(`${base}/schedules/${encodeURIComponent(scheduleId)}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ action: "cancel" }),
        }).catch(() => null);
        if (!settled(res)) {
          setLaunch((cur) => ({
            ...(cur ?? { phase: "schedule" }),
            error: "Couldn’t stop the half-launched patrol. Try again, or open the chat and stop it from the Patrol bar.",
            conversationId,
          }));
          return false;
        }
        setCreated({ conversationId, scheduleId: "" });
      }
      const res = await fetch(base, { method: "DELETE" }).catch(() => null);
      if (!settled(res)) {
        setLaunch((cur) => ({
          ...(cur ?? { phase: "conversation" }),
          error: "Couldn’t remove the half-created patrol chat. Try again, or open it and delete it from Chat.",
          conversationId,
        }));
        return false;
      }
    }
    setCreated({ conversationId: "", scheduleId: "" });
    setLaunch(null);
    return true;
  };

  // While requests are in flight the dialog cannot be dismissed: closing it
  // would not stop the launch, only hide its outcome.
  // A launch or an AI draft in flight: closing would not stop either, only
  // hide its outcome (and a draft holds the one-per-user draft slot).
  const inFlight = drafting || (launch !== null && launch.phase !== "done" && !launch.error);
  const close = () => {
    if (inFlight) return;
    // Closing on a failed, half-created launch rolls it back first, exactly
    // as Back to the plan does — dismissal must not leave a chat (or an
    // armed patrol) behind.
    if (launch?.error && (created.current.conversationId || created.current.pendingCreate)) {
      // Close only once the rollback is confirmed; if it is not, the dialog
      // stays open on the error, which says what is left and how to retry.
      void backToPlan().then((ok) => {
        if (ok) onClose();
      });
      return;
    }
    onClose();
  };

  const openPatrol = () => {
    const id = launch?.conversationId;
    onClose();
    if (id) navigate(`/chat?c=${encodeURIComponent(id)}`);
  };

  return (
    <DialogShell
      label="Patrol Launchpad"
      scrimLabel="Close the Patrol Launchpad"
      onDismiss={close}
      // Above the rail's overlay drawer (640–900px), which it is opened from.
      layer="rail"
      testId="patrol-launchpad"
      // On a phone it is a full-height sheet, like the Tools library.
      sheetOnPhone
      className="flex h-[100dvh] flex-col overflow-hidden sm:h-auto sm:max-h-[calc(100dvh-2rem)] sm:max-w-[46rem]"
    >
      <LaunchpadHeader stepIndex={stepIndex} launching={launch !== null} closable={!inFlight} onClose={close} />
      {launch ? (
        <LaunchProgress
          launch={launch}
          mode={state.mode}
          cadence={cadenceText}
          title={savedTitle}
          onOpen={openPatrol}
          onBack={() => void backToPlan()}
        />
      ) : (
        <>
          <div ref={bodyRef} className="min-h-0 flex-1 overflow-y-auto px-5 pb-5 pt-4 sm:px-7" data-testid={`launchpad-step-${state.step}`}>
            {state.step === "why" ? <WhyStep readOnly={readOnly} /> : null}
            {state.step === "welcome" ? (
              <WelcomeStep
                connectors={connectors}
                memories={memories}
                memoriesFailed={memoriesFailed}
                launchedBefore={launchedBefore()}
              />
            ) : null}
            {state.step === "tools" ? (
              <ToolsStep
                connectors={connectors}
                selected={selected}
                error={loadError}
                onToggle={(name) => {
                  const set = new Set(selected);
                  if (set.has(name)) set.delete(name);
                  else set.add(name);
                  patch({ selected: [...set] });
                }}
                onRefresh={() => void loadConnectors()}
              />
            ) : null}
            {state.step === "rules" ? (
              <RulesStep
                persona={state.persona}
                onPersona={(persona) => patch({ persona })}
                defaultPersona={defaultPersona}
                workspacePersonas={workspacePersonas}
                userPersonas={userPersonas}
                memories={memories}
                onMemoriesChanged={() => void loadMemories()}
                onMemoryAdded={(mem) => {
                  setState((cur) => ({ ...cur, memoriesAdded: cur.memoriesAdded + 1 }));
                  // Into the roster now, not after the refresh: the AI draft
                  // must see it even if that refresh is slow or fails.
                  memoriesGen.current++;
                  setMemories((cur) => (cur ? withMemory(cur, mem) : cur));
                }}
              />
            ) : null}
            {state.step === "watch" ? (
              <WatchStep
                goals={state.goals}
                notes={state.notes}
                mode={state.mode}
                connectors={(connectors ?? []).filter((c) => selected.includes(c.name))}
                onGoals={(goals) => patch({ goals })}
                onNotes={(notes) => patch({ notes })}
                onMode={(mode) => patch({ mode })}
              />
            ) : null}
            {state.step === "schedule" ? (
              <ScheduleStep
                cron={state.cron}
                timezone={state.timezone}
                onCron={(cron) => patch({ cron })}
                onTimezone={(timezone) => patch({ timezone })}
              />
            ) : null}
            {state.step === "launch" ? (
              <LaunchStep
                title={title}
                prompt={prompt}
                summary={state.summary}
                persona={personaName(state.persona, defaultPersona, userPersonas)}
                tools={(connectors ?? []).filter((c) => selected.includes(c.name) && usable(c)).map(connectorLabel)}
                goals={state.goals}
                notes={state.notes}
                mode={state.mode}
                cadence={cadenceText}
                onEdit={(p) =>
                  patch(
                    p.title !== undefined
                      ? { title: p.title, titleEdited: true }
                      : { prompt: p.prompt ?? "", promptEdited: true, promptBasis: state.promptEdited ? state.promptBasis : template },
                  )
                }
                promptStale={promptStale}
                memoriesAdded={state.memoriesAdded}
                // Resets the instructions only: a name the person chose stays.
                onReset={() => patch({ promptEdited: false, prompt: "", promptBasis: "", summary: "" })}
                memoriesUnknown={memories === null}
                memoriesFailed={memoriesFailed}
                onRetryMemories={() => void loadMemories()}
                draftRequest={{
                  goals: PATROL_GOALS.filter((g) => state.goals.includes(g.id)).map((g) => `${g.title}: ${g.step}`),
                  notes: state.notes,
                  connectors: (connectors ?? [])
                    .filter((c) => selected.includes(c.name) && usable(c))
                    .map((c) => ({ name: connectorLabel(c), description: c.description ?? "" })),
                  // No memories: the server reads them itself, rendered as a
                  // patrol turn receives them (httpapi memoryContents).
                  cadence: cadenceText,
                  mode: state.mode,
                  persona: state.persona,
                }}
                drafting={drafting}
                setDrafting={setDrafting}
                onDrafted={(d) =>
                  patch({
                    title: d.title,
                    titleEdited: true,
                    prompt: d.prompt,
                    promptEdited: true,
                    promptBasis: template,
                    summary: d.summary,
                  })
                }
              />
            ) : null}
          </div>
          <footer className="flex shrink-0 items-center justify-between gap-3 border-t border-[var(--color-border)] bg-[var(--color-surface-2)] px-5 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-3 sm:px-7 sm:pb-3 [&_button]:any-pointer-coarse:min-h-11">
            {stepIndex > 0 ? (
              <button
                type="button"
                className={btnClass({ reveal: true })}
                onClick={() => go(-1)}
                // A draft in flight was asked for this plan; changing the plan
                // under it would let the stale reply land on the new one.
                disabled={drafting}
                data-testid="launchpad-back"
              >
                Back
              </button>
            ) : (
              <button type="button" className={btnClass()} onClick={onClose}>
                Maybe later
              </button>
            )}
            <span className="hidden text-xs text-[var(--color-text-muted)] sm:inline">
              Step {stepIndex + 1} of {STEPS.length}
            </span>
            {readOnly ? (
              <button type="button" className={`${btnClass({ variant: "primary" })} px-5`} onClick={onClose} data-testid="launchpad-readonly-close">
                Got it
              </button>
            ) : state.step === "launch" ? (
              <button
                type="button"
                className={`${btnClass({ variant: "primary" })} px-5`}
                onClick={() => void doLaunch()}
                disabled={drafting || !prompt.trim() || !state.cron.trim()}
                title={drafting ? "Waiting for the assistant's draft" : undefined}
                data-testid="launchpad-launch"
              >
                <Icon name="zap" className="size-4" />
                Launch patrol
              </button>
            ) : (
              <button
                type="button"
                className={`${btnClass({ variant: "primary" })} px-5`}
                onClick={() => go(1)}
                disabled={!canNext}
                data-testid="launchpad-next"
              >
                {state.step === "why" ? "Show me the plan" : state.step === "welcome" ? "Let’s go" : "Next"}
                <Icon name="arrow-right" className="size-4" />
              </button>
            )}
          </footer>
        </>
      )}
    </DialogShell>
  );
}

function launchedBefore(): boolean {
  try {
    return window.localStorage.getItem(LAUNCHPAD_LAUNCHED_ITEM) === "1";
  } catch {
    return false;
  }
}

function personaName(ref: string, fallback: string, users: UserPersona[]): string {
  const name = ref || fallback;
  if (!name) return "Workspace default";
  const user = users.find((u) => u.ref === name);
  if (user) return user.display_name;
  return name.charAt(0).toUpperCase() + name.slice(1);
}

/* ── Chrome ─────────────────────────────────────────────────────────────── */

function LaunchpadHeader({
  stepIndex,
  launching,
  closable,
  onClose,
}: {
  stepIndex: number;
  launching: boolean;
  closable: boolean;
  onClose: () => void;
}) {
  return (
    <header className="relative shrink-0 overflow-hidden border-b border-[var(--color-border)] px-5 pb-4 pt-[max(1.25rem,env(safe-area-inset-top))] sm:px-7 sm:pt-5">
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 bg-[radial-gradient(120%_140%_at_0%_0%,color-mix(in_srgb,var(--color-primary)_22%,transparent),transparent_60%),radial-gradient(90%_120%_at_100%_0%,color-mix(in_srgb,var(--color-accent)_18%,transparent),transparent_55%)]"
      />
      <div className="relative flex items-start justify-between gap-3">
        <div className="flex items-center gap-3">
          <span className="inline-flex size-10 shrink-0 items-center justify-center rounded-xl bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] shadow-[var(--shadow-md)]">
            <Icon name="sparkles" className="size-5" />
          </span>
          <div>
            <h2 className="text-lg font-semibold leading-tight text-[var(--color-text-primary)]">Patrol Launchpad</h2>
            <p className="hidden text-[0.82rem] text-[var(--color-text-secondary)] sm:block">
              Set up a new patrol in about two minutes. Once launched, it lives in Chat.
            </p>
          </div>
        </div>
        <button
          type="button"
          aria-label="Close the Patrol Launchpad"
          onClick={onClose}
          disabled={!closable}
          title={closable ? undefined : "Launching — this takes a few seconds"}
          className="inline-flex size-8 any-pointer-coarse:size-11 shrink-0 items-center disabled:cursor-wait disabled:opacity-40 justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          <Icon name="close" className="size-4" />
        </button>
      </div>
      <ol className="relative mt-4 grid grid-cols-7 gap-1.5" aria-label="Launchpad steps">
        {STEPS.map((s, i) => {
          const done = launching || i < stepIndex;
          const current = !launching && i === stepIndex;
          return (
            <li key={s.id} className="min-w-0" aria-current={current ? "step" : undefined}>
              <div
                className={`h-1.5 rounded-full transition-colors duration-300 ${
                  done || current
                    ? "bg-[var(--color-primary-fill)]"
                    : "bg-[var(--color-border)]"
                } ${current ? "motion-safe:animate-pulse" : ""}`}
              />
              <span
                className={`mt-1.5 hidden items-center gap-1 truncate text-[0.7rem] sm:flex ${
                  current ? "font-semibold text-[var(--color-text-primary)]" : "text-[var(--color-text-muted)]"
                }`}
              >
                <Icon name={done ? "check" : s.icon} className="size-3 shrink-0" />
                {s.label}
              </span>
              <span className="sr-only sm:hidden">
                {s.label}
                {done ? " (done)" : ""}
              </span>
            </li>
          );
        })}
      </ol>
    </header>
  );
}

function StepTitle({ kicker, title, children }: { kicker: string; title: string; children?: React.ReactNode }) {
  return (
    <div className="mb-4">
      <p className="text-[0.7rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-link)]">{kicker}</p>
      <h3 className="mt-0.5 text-[1.15rem] font-semibold text-[var(--color-text-primary)]">{title}</h3>
      {children ? <div className="mt-1 text-[0.86rem] leading-relaxed text-[var(--color-text-secondary)]">{children}</div> : null}
    </div>
  );
}

const CARD =
  "rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-2)]";

/* ── 0 · Why (the expanding mind) ───────────────────────────────────────── */

// The rungs of the expanding-mind meme. Each rung has a hand-drawn head
// (MindArt) whose brain grows, glows and finally outgrows it into a galaxy,
// and a one-line quote of what you would say to the assistant at that rung.
type MindRung = {
  title: string;
  quote: string;
  text: string;
  meme: string;
};

const MIND_RUNGS: MindRung[] = [
  {
    title: "Chat with Fleet",
    quote: "“Hey Fleet, what did we spend yesterday?”",
    text: "You ask, it answers — then waits politely for you to ask again.",
    meme: "Small brain",
  },
  {
    title: "Schedule reports",
    quote: "“Email me the pacing report every Monday.”",
    text: "The numbers show up on time. Reading them and deciding is still all you.",
    meme: "Bigger brain",
  },
  {
    title: "Patrols that analyze and decide",
    quote: "“Watch my deals. Tell me what’s broken and how to fix it.”",
    text: "It checks your tools on a schedule and brings you findings, not spreadsheets.",
    meme: "Glowing brain",
  },
  {
    title: "Let it act on your behalf",
    quote: "“Fix the routine stuff while I sleep. Brief me over coffee.”",
    text: "Routine fixes just happen — every action listed for you to check.",
    meme: "Galaxy brain",
  },
];

function WhyStep({ readOnly }: { readOnly: boolean }) {
  return (
    <div>
      <StepTitle kicker="Why patrol?" title="Where the real gains from AI come from">
        Most teams stop at the first rung. The step change comes when the assistant does the checking — and then the
        doing — on its own schedule.
      </StepTitle>
      <ol className="grid gap-2" aria-label="The expanding mind of AI adoption" data-testid="launchpad-mind">
        {MIND_RUNGS.map((r, i) => {
          const top = i === MIND_RUNGS.length - 1;
          return (
            <li
              key={r.title}
              className={`grid grid-cols-[4.75rem_1fr] items-center gap-3 overflow-hidden rounded-[var(--radius-lg)] border px-3 py-2.5 ${
                top
                  ? "border-[var(--color-primary)] bg-[radial-gradient(120%_160%_at_0%_50%,color-mix(in_srgb,var(--color-primary)_18%,transparent),transparent_70%)]"
                  : "border-[var(--color-border)] bg-[var(--color-surface-2)]"
              }`}
            >
              <span aria-hidden="true" className="flex justify-center">
                <MindArt level={(i + 1) as MindLevel} className="size-[4.5rem] overflow-visible" />
              </span>
              <span className="min-w-0">
                <span className="flex flex-wrap items-baseline justify-between gap-x-2">
                  <span className="text-[0.9rem] font-semibold text-[var(--color-text-primary)]">{r.title}</span>
                  <span className="text-[0.68rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
                    {r.meme}
                  </span>
                </span>
                <q className="block text-[0.8rem] italic text-[var(--color-text-primary)] before:content-none after:content-none">
                  {r.quote}
                </q>
                <span className="block text-[0.76rem] text-[var(--color-text-secondary)]">{r.text}</span>
              </span>
            </li>
          );
        })}
      </ol>
      <div className={`${CARD} mt-4 px-4 py-3 text-[0.82rem] leading-relaxed text-[var(--color-text-secondary)]`}>
        <p>
          <a
            className="text-[var(--color-link)] underline underline-offset-2"
            href="https://www.oneusefulthing.org/"
            target="_blank"
            rel="noopener noreferrer"
          >
            Dr. Ethan Mollick
          </a>{" "}
          — Wharton professor and author of the <em>One Useful Thing</em> newsletter — calls the gap between what AI
          can already do and what we actually use it for a{" "}
          <strong className="text-[var(--color-text-primary)]">capability overhang</strong> (
          <a
            className="text-[var(--color-link)] underline underline-offset-2"
            href="https://www.penguinrandomhouse.com/books/813026/co-existence-by-ethan-mollick/"
            target="_blank"
            rel="noopener noreferrer"
          >
            <cite>Co-Existence</cite>
          </a>
          , 2026). Patrols are how you close it: you delegate the watching, check its work,
          and hand over more as it earns your trust.
        </p>
        <p className="mt-2 flex flex-wrap items-center gap-1.5 text-[0.78rem]" data-testid="launchpad-trust-path">
          <span className="font-medium text-[var(--color-text-primary)]">You climb at your pace:</span>
          <span className="rounded-full border border-[var(--color-border)] px-2 py-0.5">Just report</span>
          <Icon name="arrow-right" className="size-3 text-[var(--color-text-muted)]" />
          <span className="rounded-full border border-[var(--color-primary)] px-2 py-0.5 text-[var(--color-text-primary)]">
            Suggest &amp; ask
          </span>
          <Icon name="arrow-right" className="size-3 text-[var(--color-text-muted)]" />
          <span className="rounded-full border border-[var(--color-border)] px-2 py-0.5">⚡ Act for me</span>
        </p>
        <p className="mt-2">
          Start where it only suggests and asks before acting. When you trust its calls, flip on ⚡ Auto-approve in the
          Patrol bar — one switch, and you can flip it back any time.
        </p>
      </div>
      {readOnly ? (
        <p
          role="status"
          className="mt-3 flex items-start gap-2 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] px-3 py-2 text-[0.82rem] text-[var(--color-text-primary)]"
          data-testid="launchpad-readonly"
        >
          <Icon name="lock" className="mt-0.5 size-3.5 shrink-0 text-[var(--color-warning)]" />
          Your account is read-only, so you can’t save memories or start a patrol here. Ask an admin for member access,
          then come back to the Launchpad.
        </p>
      ) : null}
    </div>
  );
}

/* ── 1 · Welcome ────────────────────────────────────────────────────────── */

function WelcomeStep({
  connectors,
  memories,
  memoriesFailed,
  launchedBefore,
}: {
  connectors: LaunchpadConnector[] | null;
  memories: Memory[] | null;
  memoriesFailed: boolean;
  launchedBefore: boolean;
}) {
  const ready = connectors?.filter((c) => usable(c)).length ?? null;
  const flow = [
    { icon: "plug", title: "Hook up your tools", text: "Email, deals, reporting — whatever you check every day." },
    { icon: "brain", title: "Set your ground rules", text: "Non-negotiables it remembers in every chat." },
    { icon: "target", title: "Pick what to watch", text: "Misconfigured deals, spend drops, missing reports, your inbox…" },
    { icon: "clock", title: "Choose a rhythm", text: "Every morning, every two hours — you decide." },
    { icon: "zap", title: "Launch", text: "The first check runs right away, then it reports back on schedule." },
  ];
  return (
    <div>
      <StepTitle kicker={launchedBefore ? "Another patrol" : "Here’s the plan"} title="Five steps, about two minutes">
        A <strong className="text-[var(--color-text-primary)]">patrol</strong> is a check the assistant runs on a schedule,
        using your connected tools. Each patrol has its own chat, and every run reports back there with what it found,
        what it recommends, and — if you let it — what it already fixed.
      </StepTitle>
      <ol className="grid gap-2">
        {flow.map((f, i) => (
          <li key={f.title} className={`${CARD} flex items-center gap-3 px-3.5 py-2.5`}>
            <span className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-primary)]">
              <Icon name={f.icon} className="size-4" />
            </span>
            <div className="min-w-0">
              <p className="text-[0.88rem] font-medium text-[var(--color-text-primary)]">
                <span className="mr-1.5 text-[var(--color-text-muted)]">{i + 1}.</span>
                {f.title}
              </p>
              <p className="text-[0.8rem] text-[var(--color-text-secondary)]">{f.text}</p>
            </div>
          </li>
        ))}
      </ol>
      <div className="mt-4 flex flex-wrap gap-2 text-[0.78rem]" data-testid="launchpad-readiness">
        <ReadyChip ok={(ready ?? 0) > 0} loading={ready === null}>
          {ready === null ? "Checking tools…" : `${ready} tool${ready === 1 ? "" : "s"} ready`}
        </ReadyChip>
        <ReadyChip ok={(memories?.length ?? 0) > 0} loading={memories === null && !memoriesFailed}>
          {memories === null
            ? memoriesFailed
              ? "Couldn’t check memories"
              : "Checking memories…"
            : `${memories.length} memor${memories.length === 1 ? "y" : "ies"} saved`}
        </ReadyChip>
      </div>
      <WhereItLives />
    </div>
  );
}

// WhereItLives answers the question the Launchpad otherwise invites: "where
// are my patrols?" Not here. This wizard only sets new ones up; each patrol is
// an ordinary chat that wakes on its schedule, so it lives in the Chat list,
// with the Patrol bar on top for Run now, Pause and Stop.
function WhereItLives({ title }: { title?: string }) {
  return (
    <p
      className={`${CARD} mt-4 flex items-start gap-2.5 px-3.5 py-2.5 text-[0.8rem] leading-relaxed text-[var(--color-text-secondary)]`}
      data-testid="launchpad-where"
    >
      <Icon name="message" className="mt-0.5 size-4 shrink-0 text-[var(--color-primary)]" />
      <span>
        <strong className="text-[var(--color-text-primary)]">Where does a patrol live?</strong> In Chat. Each patrol is
        a chat that wakes up on its own schedule — after launch you’ll find it in your chat list
        {title ? (
          <>
            {" "}
            as <strong className="text-[var(--color-text-primary)]">{title}</strong>
          </>
        ) : null}
        , with the Patrol bar on top to run, pause or stop it. This Launchpad only sets new patrols up.{" "}
        <a
          className="text-[var(--color-link)] underline underline-offset-2"
          href="/help/chat#in-chat-patrols-and-timers"
          target="_blank"
          rel="noopener"
        >
          How patrols work
        </a>
      </span>
    </p>
  );
}

function ReadyChip({ ok, loading, children }: { ok: boolean; loading: boolean; children: React.ReactNode }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 ${
        ok
          ? "border-[color-mix(in_srgb,var(--color-success)_45%,var(--color-border))] text-[var(--color-text-primary)]"
          : "border-[var(--color-border)] text-[var(--color-text-secondary)]"
      }`}
    >
      <span
        aria-hidden="true"
        className={`size-1.5 rounded-full ${
          loading ? "bg-[var(--color-text-muted)]" : ok ? "bg-[var(--color-success)]" : "bg-[var(--color-warning)]"
        }`}
      />
      {children}
    </span>
  );
}

/* ── 2 · Tools ──────────────────────────────────────────────────────────── */

function ToolsStep({
  connectors,
  selected,
  error,
  onToggle,
  onRefresh,
}: {
  connectors: LaunchpadConnector[] | null;
  selected: string[];
  error: string | null;
  onToggle: (name: string) => void;
  onRefresh: () => void;
}) {
  const ready = (connectors ?? []).filter(usable);
  const setup = (connectors ?? []).filter((c) => c.status === "needs_setup");
  return (
    <div>
      <StepTitle kicker="Your tools" title="Hook up your tools">
        A patrol can only look where it has access. Tick the tools this patrol should use — you can change them later
        from the chat’s Tools menu.
      </StepTitle>
      {error ? <p className="mb-3 text-[0.82rem] text-[var(--color-danger)]">{error}</p> : null}
      {connectors === null ? (
        <p className="py-6 text-center text-[0.85rem] text-[var(--color-text-muted)]">Loading your tools…</p>
      ) : (
        <>
          {ready.length === 0 ? (
            <div className={`${CARD} px-4 py-5 text-center text-[0.85rem] text-[var(--color-text-secondary)]`}>
              No tools are connected yet. Connect one below — the patrol can still use the assistant’s built-in tools
              meanwhile.
            </div>
          ) : (
            <ul className="grid gap-2 sm:grid-cols-2" aria-label="Ready to use">
              {ready.map((c) => {
                const on = c.always_on || selected.includes(c.name);
                return (
                  <li key={c.name}>
                    <label
                      className={`flex h-full cursor-pointer items-start gap-3 rounded-[var(--radius-lg)] border px-3 py-2.5 transition ${
                        on
                          ? "border-[var(--color-primary)] bg-[color-mix(in_srgb,var(--color-primary)_8%,var(--color-surface-2))]"
                          : "border-[var(--color-border)] bg-[var(--color-surface-2)] hover:border-[var(--color-border-strong)]"
                      } ${c.always_on ? "cursor-default" : ""}`}
                    >
                      <input
                        type="checkbox"
                        className="mt-0.5 size-4 shrink-0 accent-[var(--color-primary)]"
                        checked={on}
                        disabled={c.always_on}
                        onChange={() => onToggle(c.name)}
                        data-testid={`launchpad-tool-${c.name}`}
                      />
                      <span className="min-w-0">
                        <span className="flex items-center gap-1.5 text-[0.88rem] font-medium text-[var(--color-text-primary)]">
                          <span className="truncate">{connectorLabel(c)}</span>
                          {c.always_on ? (
                            <span className="shrink-0 rounded-full bg-[var(--color-overlay-soft)] px-1.5 py-px text-[0.66rem] font-normal text-[var(--color-text-muted)]">
                              Always on
                            </span>
                          ) : null}
                        </span>
                        {c.description ? (
                          <span className="line-clamp-2 text-[0.78rem] text-[var(--color-text-secondary)]">{c.description}</span>
                        ) : null}
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>
          )}
          {setup.length > 0 ? (
            <div className="mt-4">
              <p className="mb-2 text-[0.78rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
                Needs setup
              </p>
              <ul className="grid gap-1.5">
                {setup.map((c) => (
                  <li key={c.name} className={`${CARD} flex items-center justify-between gap-3 px-3 py-2`}>
                    <span className="min-w-0 truncate text-[0.85rem] text-[var(--color-text-secondary)]">{connectorLabel(c)}</span>
                    <a
                      href={`/settings/connections?connector=${encodeURIComponent(c.name)}`}
                      target="_blank"
                      rel="noopener"
                      className={btnClass({ sm: true, reveal: true })}
                    >
                      Set up
                      <Icon name="external-link" className="size-3" />
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          <div className="mt-4 flex flex-wrap items-center gap-2">
            <a href="/settings/connections" target="_blank" rel="noopener" className={btnClass({ sm: true, reveal: true })}>
              <Icon name="plus" className="size-3.5" />
              Connect another tool
              <Icon name="external-link" className="size-3" />
            </a>
            <button type="button" className={btnClass({ sm: true })} onClick={onRefresh}>
              <Icon name="refresh" className="size-3.5" />
              Refresh
            </button>
            <span className="text-[0.75rem] text-[var(--color-text-muted)]">Opens in a new tab; this list refreshes when you come back.</span>
          </div>
        </>
      )}
    </div>
  );
}

/* ── 3 · Ground rules ───────────────────────────────────────────────────── */

function RulesStep({
  persona,
  onPersona,
  defaultPersona,
  workspacePersonas,
  userPersonas,
  memories,
  onMemoriesChanged,
  onMemoryAdded,
}: {
  persona: string;
  onPersona: (p: string) => void;
  defaultPersona: string;
  workspacePersonas: string[];
  userPersonas: UserPersona[];
  memories: Memory[] | null;
  onMemoriesChanged: () => void;
  onMemoryAdded: (mem: Memory) => void;
}) {
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [justAdded, setJustAdded] = useState<string[]>([]);
  const others = workspacePersonas.filter((p) => p !== defaultPersona);

  const add = async (content: string) => {
    const text = content.trim();
    if (!text || busy) return;
    // Identical text is the same rule: never save it twice. This also makes a
    // retry after an unconfirmed save safe once the roster shows the first.
    const same = (m: Memory) => m.content.trim().toLowerCase() === text.toLowerCase();
    if ((memories ?? []).some(same)) {
      setDraft((cur) => (cur.trim() === text ? "" : cur));
      setError("That one is already saved.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await fetch("/api/memories", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content: text, kind: "constraint" }),
      }).catch(() => null);
      if (res && !res.ok) throw new Error(await stepErrorText(res, "Could not save that"));
      let savedId = res ? await res.json().then((d: { id?: string }) => d?.id ?? "").catch(() => "") : "";
      // No reply (or an unreadable one): the save may still have committed.
      // Look before inviting a retry, so it is never saved twice.
      if (!savedId) {
        savedId = await fetch("/api/memories", { cache: "no-store" })
          .then((r) => (r.ok ? (r.json() as Promise<{ memories?: Memory[] }>) : { memories: [] }))
          .then((d) => activeMemories(d.memories ?? []).find(same)?.id ?? "")
          .catch(() => "");
        if (!savedId) throw new Error("Couldn’t confirm that was saved. Check your connection and try again.");
      }
      const mem = { id: savedId };
      // Pinned: a non-negotiable is never retired by the automatic
      // memory upkeep (it can still be edited or deleted in Settings).
      // Clear the box only if it still holds what was saved: the person may
      // already be typing (or have picked) their next rule.
      setDraft((cur) => (cur.trim() === text ? "" : cur));
      if (mem.id) {
        setJustAdded((cur) => [...cur, mem.id as string]);
        const pinRes = await fetch(`/api/memories/${encodeURIComponent(mem.id)}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ pinned: true }),
        }).catch(() => null);
        // Into the roster as what the store now holds: pinned only once the
        // pin is confirmed, so its place in the order matches a turn's.
        onMemoryAdded({ id: mem.id, content: text, kind: "constraint", pinned: !!pinRes?.ok });
        // Saved but not pinned: say so, since an unpinned memory can be
        // retired by the automatic upkeep.
        if (!pinRes?.ok) {
          setError("Saved, but it could not be pinned — pin it in Settings → Memories so it is never retired.");
        }
      }
      onMemoriesChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not save that.");
    } finally {
      setBusy(false);
    }
  };

  const existing = memories ?? [];
  const taken = new Set(existing.map((m) => m.content.trim().toLowerCase()));
  const suggestions = MEMORY_SUGGESTIONS.filter((s) => !taken.has(s.toLowerCase()));

  return (
    <div>
      <StepTitle kicker="Ground rules" title="Set your ground rules" />

      <section className={`${CARD} mb-4 px-4 py-3`} aria-labelledby="launchpad-persona-h">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h4 id="launchpad-persona-h" className="flex items-center gap-1.5 text-[0.9rem] font-semibold text-[var(--color-text-primary)]">
            <Icon name="persona" className="size-4 text-[var(--color-primary)]" />
            Who’s on watch
          </h4>
          <select
            aria-label="Persona for this patrol"
            className={`${SETTINGS_INPUT} min-h-[2.2rem] w-auto py-1 text-[0.85rem]`}
            value={persona}
            onChange={(e) => onPersona(e.target.value)}
            data-testid="launchpad-persona"
          >
            <option value="">{defaultPersona ? `${cap(defaultPersona)} (workspace default)` : "Workspace default"}</option>
            {others.map((p) => (
              <option key={p} value={p}>
                {cap(p)}
              </option>
            ))}
            {userPersonas.map((p) => (
              <option key={p.ref} value={p.ref}>
                {p.display_name} (yours)
              </option>
            ))}
          </select>
        </div>
        <p className="mt-1.5 text-[0.8rem] leading-relaxed text-[var(--color-text-secondary)]">
          A persona shapes how the assistant thinks and writes. The workspace default is a great start; later you can build
          a <em>digital twin</em> that decides the way you do in{" "}
          <a className="text-[var(--color-link)] underline underline-offset-2" href="/settings/personas" target="_blank" rel="noopener">
            Settings → Personas
          </a>
          .
        </p>
      </section>

      <section aria-labelledby="launchpad-memories-h">
        <h4 id="launchpad-memories-h" className="flex items-center gap-1.5 text-[0.9rem] font-semibold text-[var(--color-text-primary)]">
          <Icon name="brain" className="size-4 text-[var(--color-primary)]" />
          Your non-negotiables
        </h4>
        <p className="mt-1 text-[0.8rem] leading-relaxed text-[var(--color-text-secondary)]">
          Saved as pinned memories: the assistant reads them in <strong>every</strong> chat, patrols included. Tap a
          suggestion or write your own.
        </p>
        <div className="mt-3 flex flex-wrap gap-1.5">
          {suggestions.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => setDraft(s)}
              className="rounded-full border border-dashed border-[var(--color-border-strong)] px-2.5 py-1 text-left text-[0.76rem] text-[var(--color-text-secondary)] transition hover:border-[var(--color-primary)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              + {s}
            </button>
          ))}
        </div>
        <form
          className="mt-3 flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void add(draft);
          }}
        >
          <input
            className={SETTINGS_INPUT}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="e.g. Never pause a campaign without asking me"
            aria-label="New non-negotiable"
            maxLength={500}
            data-testid="launchpad-memory-input"
          />
          <button type="submit" className={`${btnClass({ variant: "primary" })} shrink-0`} disabled={busy || !draft.trim()} data-testid="launchpad-memory-add">
            {busy ? "Saving…" : "Save"}
          </button>
        </form>
        {error ? <p className="mt-2 text-[0.8rem] text-[var(--color-danger)]">{error}</p> : null}
        {existing.length > 0 ? (
          <ul className="mt-3 grid gap-1.5" aria-label="Saved memories" data-testid="launchpad-memories">
            {existing.slice(0, 8).map((m) => (
              <li key={m.id} className="flex items-start gap-2 text-[0.82rem] text-[var(--color-text-secondary)]">
                <Icon
                  name="check"
                  className={`mt-0.5 size-3.5 shrink-0 ${justAdded.includes(m.id) ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]"}`}
                />
                <span className="min-w-0">{m.content}</span>
              </li>
            ))}
            {existing.length > 8 ? (
              <li className="text-[0.78rem] text-[var(--color-text-muted)]">
                …and {existing.length - 8} more in{" "}
                <a className="text-[var(--color-link)] underline underline-offset-2" href="/settings/memories" target="_blank" rel="noopener">
                  Settings → Memories
                </a>
              </li>
            ) : null}
          </ul>
        ) : null}
      </section>
    </div>
  );
}

function cap(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

/* ── 4 · Watch list ─────────────────────────────────────────────────────── */

function WatchStep({
  goals,
  notes,
  mode,
  connectors,
  onGoals,
  onNotes,
  onMode,
}: {
  goals: string[];
  notes: string;
  mode: PatrolMode;
  connectors: LaunchpadConnector[];
  onGoals: (g: string[]) => void;
  onNotes: (n: string) => void;
  onMode: (m: PatrolMode) => void;
}) {
  const toggle = (id: string) => onGoals(goals.includes(id) ? goals.filter((g) => g !== id) : [...goals, id]);
  return (
    <div>
      <StepTitle kicker="Watch list" title="What should it keep an eye on?">
        Pick everything you check by hand today. Tools that fit each check are shown — pick it anyway if none do, and
        the patrol will tell you what it needs.
      </StepTitle>
      <ul className="grid gap-2 sm:grid-cols-2" aria-label="Things to watch">
        {PATROL_GOALS.map((g) => {
          const on = goals.includes(g.id);
          const using = matchingConnectors(g, connectors);
          return (
            <li key={g.id}>
              <button
                type="button"
                aria-pressed={on}
                onClick={() => toggle(g.id)}
                data-testid={`launchpad-goal-${g.id}`}
                className={`flex h-full w-full items-start gap-3 rounded-[var(--radius-lg)] border px-3 py-2.5 text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
                  on
                    ? "border-[var(--color-primary)] bg-[color-mix(in_srgb,var(--color-primary)_9%,var(--color-surface-2))]"
                    : "border-[var(--color-border)] bg-[var(--color-surface-2)] hover:border-[var(--color-border-strong)]"
                }`}
              >
                <span
                  className={`inline-flex size-8 shrink-0 items-center justify-center rounded-lg transition ${
                    on
                      ? "bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)]"
                      : "bg-[color-mix(in_srgb,var(--color-primary)_12%,transparent)] text-[var(--color-primary)]"
                  }`}
                >
                  <Icon name={on ? "check" : g.icon} className="size-4" />
                </span>
                <span className="min-w-0">
                  <span className="block text-[0.88rem] font-medium text-[var(--color-text-primary)]">{g.title}</span>
                  <span className="block text-[0.78rem] text-[var(--color-text-secondary)]">{g.blurb}</span>
                  <span className="mt-1 block text-[0.72rem] text-[var(--color-text-secondary)]">
                    {using.length > 0 ? `Uses ${using.map(connectorLabel).slice(0, 3).join(", ")}` : `Needs ${g.needs}`}
                  </span>
                </span>
              </button>
            </li>
          );
        })}
      </ul>
      <label className="mt-4 block">
        <span className="text-[0.85rem] font-medium text-[var(--color-text-primary)]">Anything else?</span>
        <textarea
          className={`${SETTINGS_INPUT} mt-1.5 min-h-[4.5rem] resize-y`}
          value={notes}
          onChange={(e) => onNotes(e.target.value)}
          placeholder="e.g. Pay extra attention to the Acme CTV deals, and tell me if any client email has gone unanswered for a day."
          maxLength={2000}
          data-testid="launchpad-notes"
        />
      </label>
      <fieldset className="mt-4">
        <legend className="text-[0.85rem] font-medium text-[var(--color-text-primary)]">How hands-on should it be?</legend>
        <div className="mt-2 grid gap-2 sm:grid-cols-3">
          {MODE_OPTIONS.map((m) => (
            <label
              key={m.id}
              className={`flex cursor-pointer flex-col gap-0.5 rounded-[var(--radius-lg)] border px-3 py-2.5 transition ${
                mode === m.id
                  ? "border-[var(--color-primary)] bg-[color-mix(in_srgb,var(--color-primary)_9%,var(--color-surface-2))]"
                  : "border-[var(--color-border)] bg-[var(--color-surface-2)] hover:border-[var(--color-border-strong)]"
              }`}
            >
              <span className="flex items-center gap-2 text-[0.86rem] font-medium text-[var(--color-text-primary)]">
                <input
                  type="radio"
                  name="launchpad-mode"
                  value={m.id}
                  checked={mode === m.id}
                  onChange={() => onMode(m.id)}
                  className="accent-[var(--color-primary)]"
                  data-testid={`launchpad-mode-${m.id}`}
                />
                {m.title}
              </span>
              <span className="text-[0.76rem] text-[var(--color-text-secondary)]">{m.blurb}</span>
            </label>
          ))}
        </div>
        <p className="mt-2 flex items-start gap-1.5 text-[0.78rem] leading-relaxed text-[var(--color-text-secondary)]" data-testid="launchpad-mode-nudge">
          <Icon name="info" className="mt-0.5 size-3.5 shrink-0 text-[var(--color-text-muted)]" />
          {mode === "act"
            ? "Bold — and you can always step back: turn ⚡ Auto-approve off in the Patrol bar and it goes back to asking first."
            : "Most people start here and graduate: once its recommendations keep being right, turn on ⚡ Auto-approve in the Patrol bar and let it act."}
        </p>
      </fieldset>
    </div>
  );
}

/* ── 5 · Schedule ───────────────────────────────────────────────────────── */

function ScheduleStep({
  cron,
  timezone,
  onCron,
  onTimezone,
}: {
  cron: string;
  timezone: string;
  onCron: (c: string) => void;
  onTimezone: (t: string) => void;
}) {
  const [custom, setCustom] = useState(() => !CADENCE_PRESETS.some((p) => p.cron === cron.trim()));
  return (
    <div>
      <StepTitle kicker="Rhythm" title="How often should it patrol?">
        The first check runs the moment you launch. After that it wakes on this rhythm and reports back in the same chat.
      </StepTitle>
      <div className="grid gap-2 sm:grid-cols-3" role="radiogroup" aria-label="Patrol rhythm">
        {CADENCE_PRESETS.map((p) => {
          const on = !custom && cron.trim() === p.cron;
          return (
            <button
              key={p.id}
              type="button"
              role="radio"
              aria-checked={on}
              onClick={() => {
                setCustom(false);
                onCron(p.cron);
              }}
              data-testid={`launchpad-cadence-${p.id}`}
              className={`rounded-[var(--radius-lg)] border px-3 py-2.5 text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
                on
                  ? "border-[var(--color-primary)] bg-[color-mix(in_srgb,var(--color-primary)_9%,var(--color-surface-2))]"
                  : "border-[var(--color-border)] bg-[var(--color-surface-2)] hover:border-[var(--color-border-strong)]"
              }`}
            >
              <span className="block text-[0.86rem] font-medium text-[var(--color-text-primary)]">{p.label}</span>
              <span className="block text-[0.75rem] text-[var(--color-text-secondary)]">{p.hint}</span>
            </button>
          );
        })}
      </div>
      <div className="mt-3">
        <button
          type="button"
          aria-expanded={custom}
          onClick={() => {
            // Leaving custom mode must leave a preset selected: otherwise no
            // card is ticked yet the custom schedule would still launch.
            if (custom && !CADENCE_PRESETS.some((p) => p.cron === cron.trim())) onCron(CADENCE_PRESETS[0].cron);
            setCustom((c) => !c);
          }}
          className={btnClass({ sm: true, reveal: true })}
          data-testid="launchpad-cadence-custom"
        >
          <Icon name="calendar" className="size-3.5" />
          {custom ? "Use a preset" : "Custom schedule"}
        </button>
      </div>
      <div className={`${CARD} mt-3 px-4 py-3`}>
        {custom ? (
          <PatrolSchedulePicker
            cron={cron}
            timezone={timezone}
            onChangeCron={onCron}
            onChangeTimezone={onTimezone}
            testIdPrefix="launchpad-schedule"
          />
        ) : (
          <p className="text-[0.82rem] text-[var(--color-text-secondary)]">
            <Icon name="clock" className="mr-1.5 inline size-3.5 align-[-2px] text-[var(--color-primary)]" />
            {describeCronExpression(cron) || cron} · {timezone || "UTC"}
            <button type="button" className="ml-2 text-[var(--color-link)] underline underline-offset-2" onClick={() => setCustom(true)}>
              Change time zone
            </button>
          </p>
        )}
      </div>
    </div>
  );
}

/* ── 6 · Review & launch ────────────────────────────────────────────────── */

type DraftRequest = {
  goals: string[];
  notes: string;
  connectors: { name: string; description: string }[];
  cadence: string;
  mode: PatrolMode;
  persona: string;
};

function LaunchStep({
  title,
  prompt,
  summary,
  persona,
  tools,
  goals,
  notes,
  mode,
  cadence,
  onEdit,
  onReset,
  promptStale,
  memoriesAdded,
  memoriesUnknown,
  memoriesFailed,
  onRetryMemories,
  draftRequest,
  onDrafted,
  drafting,
  setDrafting,
}: {
  title: string;
  prompt: string;
  summary: string;
  persona: string;
  tools: string[];
  goals: string[];
  notes: string;
  mode: PatrolMode;
  cadence: string;
  onEdit: (p: { title?: string; prompt?: string }) => void;
  onReset: () => void;
  promptStale: boolean;
  memoriesAdded: number;
  // The memory roster is not loaded (yet, or it failed): the AI draft waits,
  // because it is told the memories so it does not contradict them.
  memoriesUnknown: boolean;
  memoriesFailed: boolean;
  onRetryMemories: () => void;
  draftRequest: DraftRequest;
  onDrafted: (d: { title: string; prompt: string; summary: string }) => void;
  // Owned by the parent so Launch waits for a draft in flight.
  drafting: boolean;
  setDrafting: (v: boolean) => void;
}) {
  const [draftError, setDraftError] = useState<string | null>(null);
  const watching = PATROL_GOALS.filter((g) => goals.includes(g.id)).map((g) => g.title);
  if (notes.trim()) watching.push("Your extra notes");

  const draftWithAI = async () => {
    setDrafting(true);
    setDraftError(null);
    try {
      const res = await fetch("/api/patrol-draft", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(draftRequest),
      });
      if (!res.ok) throw new Error(await stepErrorText(res, "The assistant could not draft this patrol"));
      const d = (await res.json()) as { title?: string; prompt?: string; summary?: string };
      if (!d.prompt?.trim()) throw new Error("The assistant returned an empty draft.");
      onDrafted({ title: d.title?.trim() || title, prompt: d.prompt.trim(), summary: d.summary?.trim() ?? "" });
    } catch (e) {
      setDraftError(
        `${e instanceof Error ? e.message : "The assistant could not draft this patrol"}. Your template draft is still here — it works as-is.`,
      );
    } finally {
      setDrafting(false);
    }
  };

  const rows: { icon: string; label: string; value: string }[] = [
    { icon: "persona", label: "On watch", value: persona },
    { icon: "plug", label: "Tools", value: tools.length ? tools.join(", ") : "Built-in tools only" },
    { icon: "target", label: "Watching", value: watching.join(" · ") || "—" },
    { icon: "clock", label: "Rhythm", value: cadence },
    { icon: "shield", label: "Hands-on", value: MODE_OPTIONS.find((m) => m.id === mode)?.title ?? mode },
  ];

  return (
    <div>
      <StepTitle kicker="Final check" title="Ready for launch">
        Check the plan, polish the instructions if you like, then launch. The patrol and its chat are only
        created when you press <strong className="text-[var(--color-text-primary)]">Launch patrol</strong>
        {memoriesAdded > 0
          ? ` — the ${memoriesAdded === 1 ? "non-negotiable" : `${memoriesAdded} non-negotiables`} you saved on Ground rules ${memoriesAdded === 1 ? "is" : "are"} already in your memories.`
          : "."}
      </StepTitle>
      <WhereItLives title={title.trim() || "My patrol"} />
      <div className="h-3" aria-hidden="true" />
      <dl className={`${CARD} grid gap-2 px-4 py-3`} data-testid="launchpad-summary">
        {rows.map((r) => (
          <div key={r.label} className="grid grid-cols-[1.25rem_5.5rem_1fr] items-start gap-2 text-[0.83rem]">
            <Icon name={r.icon} className="mt-0.5 size-3.5 text-[var(--color-primary)]" />
            <dt className="text-[var(--color-text-muted)]">{r.label}</dt>
            <dd className="min-w-0 text-[var(--color-text-primary)]">{r.value}</dd>
          </div>
        ))}
      </dl>

      <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
        <label htmlFor="launchpad-title" className="text-[0.85rem] font-medium text-[var(--color-text-primary)]">
          Patrol name
        </label>
      </div>
      <input
        id="launchpad-title"
        className={`${SETTINGS_INPUT} mt-1.5`}
        value={title}
        maxLength={80}
        onChange={(e) => onEdit({ title: e.target.value })}
        // Read-only while the assistant drafts: its reply replaces both
        // fields, and an edit made meanwhile would be silently lost.
        readOnly={drafting}
        aria-busy={drafting}
        data-testid="launchpad-title"
      />

      <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
        <label htmlFor="launchpad-prompt" className="text-[0.85rem] font-medium text-[var(--color-text-primary)]">
          What it does each run
        </label>
        <div className="flex items-center gap-1.5">
          <button type="button" className={btnClass({ sm: true })} onClick={onReset} disabled={drafting}>
            Reset to template
          </button>
          <button
            type="button"
            onClick={() => void draftWithAI()}
            disabled={drafting || memoriesUnknown}
            title={memoriesUnknown ? "Waiting for your memories, so the draft cannot contradict them" : undefined}
            data-testid="launchpad-draft-ai"
            className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-primary)_55%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-primary)_12%,var(--color-surface-1))] px-3 py-[0.3rem] text-[0.78rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-primary)] hover:bg-[color-mix(in_srgb,var(--color-primary)_20%,var(--color-surface-1))] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-wait disabled:opacity-70"
          >
            <Icon name="sparkles" className={`size-3.5 text-[var(--color-primary)] ${drafting ? "motion-safe:animate-spin" : ""}`} />
            {drafting ? "Drafting…" : "Draft it with AI"}
          </button>
        </div>
      </div>
      {summary ? (
        <p className="mt-2 flex items-start gap-1.5 rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-primary)_8%,transparent)] px-3 py-2 text-[0.8rem] text-[var(--color-text-primary)]" data-testid="launchpad-ai-summary">
          <Icon name="sparkles" className="mt-0.5 size-3.5 shrink-0 text-[var(--color-primary)]" />
          {summary}
        </p>
      ) : null}
      {memoriesFailed && memoriesUnknown ? (
        <p className="mt-2 text-[0.78rem] text-[var(--color-text-secondary)]" data-testid="launchpad-memories-failed">
          Couldn’t load your memories, so <strong>Draft it with AI</strong> is paused (it must not contradict them).{" "}
          <button type="button" className="text-[var(--color-link)] underline underline-offset-2" onClick={onRetryMemories}>
            Try again
          </button>
        </p>
      ) : null}
      {promptStale ? (
        <p
          role="status"
          className="mt-2 flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] px-3 py-2 text-[0.8rem] text-[var(--color-text-primary)]"
          data-testid="launchpad-prompt-stale"
        >
          <Icon name="alert-triangle" className="size-3.5 shrink-0 text-[var(--color-warning)]" />
          You changed the plan after these instructions were written — they may not match it any more.
          <button type="button" className="text-[var(--color-link)] underline underline-offset-2" onClick={onReset}>
            Rewrite from the plan
          </button>
        </p>
      ) : null}
      {draftError ? (
        <p role="alert" className="mt-2 text-[0.8rem] text-[var(--color-danger)]">
          {draftError}
        </p>
      ) : null}
      <textarea
        id="launchpad-prompt"
        className={`${SETTINGS_INPUT} mt-1.5 min-h-[13rem] resize-y font-[family-name:var(--font-code-ui)] text-[0.8rem] leading-relaxed ${drafting ? "opacity-60" : ""}`}
        value={prompt}
        onChange={(e) => onEdit({ prompt: e.target.value })}
        readOnly={drafting}
        aria-busy={drafting}
        data-testid="launchpad-prompt"
      />
      <p className="mt-1.5 text-[0.75rem] text-[var(--color-text-muted)]">
        This is the message the assistant receives every time the patrol wakes. To change it later, just ask in the patrol’s chat.
      </p>
    </div>
  );
}

/* ── Launch progress & success ──────────────────────────────────────────── */

function LaunchProgress({
  launch,
  mode,
  cadence,
  title,
  onOpen,
  onBack,
}: {
  launch: LaunchState;
  mode: PatrolMode;
  cadence: string;
  title: string;
  onOpen: () => void;
  onBack: () => void;
}) {
  const done = launch.phase === "done";
  const at = done ? LAUNCH_PHASES.length : LAUNCH_PHASES.findIndex((p) => p.id === launch.phase);
  const openRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    if (done) openRef.current?.focus();
  }, [done]);
  return (
    <div className="flex min-h-[22rem] flex-1 flex-col items-center justify-center gap-5 px-6 py-8 text-center" data-testid="launchpad-launching" aria-live="polite">
      {done ? (
        <>
          <div className="relative">
            <span aria-hidden="true" className="absolute inset-0 rounded-full bg-[color-mix(in_srgb,var(--color-success)_35%,transparent)] motion-safe:animate-ping" />
            <span className="relative inline-flex size-16 items-center justify-center rounded-full bg-[var(--color-success)] text-[var(--color-bg)] shadow-[var(--shadow-md)]">
              <Icon name="check" className="size-8" />
            </span>
          </div>
          <div>
            <h3 className="text-xl font-semibold text-[var(--color-text-primary)]" data-testid="launchpad-done">
              Your patrol is live
            </h3>
            <p className="mx-auto mt-1.5 max-w-md text-[0.88rem] leading-relaxed text-[var(--color-text-secondary)]">
              {launch.firstRun === "running" ? (
                <>
                  <strong className="text-[var(--color-text-primary)]">{title}</strong> is running its first check right now.
                </>
              ) : launch.firstRun === "unknown" ? (
                <span data-testid="launchpad-first-run-unknown">
                  <strong className="text-[var(--color-text-primary)]">{title}</strong> is armed. We couldn’t confirm its
                  first check started — open the chat to see whether it is running before you press <strong>Run now</strong>.
                </span>
              ) : (
                <span data-testid="launchpad-first-run-refused">
                  <strong className="text-[var(--color-text-primary)]">{title}</strong> is armed, but its first check could
                  not start right now — press <strong>Run now</strong> in the Patrol bar, or it runs at its next scheduled time.
                </span>
              )}{" "}
              After that it reports back on schedule — {cadence}. Find it any time in your Chat list; pause or stop it from
              the Patrol bar at the top of that chat, or ask there to change it.
            </p>
            {mode !== "act" ? (
              <p className="mx-auto mt-3 max-w-md rounded-[var(--radius-md)] border border-dashed border-[var(--color-border-strong)] px-3 py-2 text-[0.8rem] text-[var(--color-text-secondary)]">
                <strong className="text-[var(--color-text-primary)]">Next rung of the ladder:</strong> when its calls keep
                being right, switch on ⚡ Auto-approve in the Patrol bar and let it act for you.
              </p>
            ) : null}
          </div>
          <button ref={openRef} type="button" className={`${btnClass({ variant: "primary" })} px-5`} onClick={onOpen} data-testid="launchpad-open">
            Watch it work
            <Icon name="arrow-right" className="size-4" />
          </button>
        </>
      ) : (
        <>
          <ol className="grid w-full max-w-sm gap-2.5 text-left">
            {LAUNCH_PHASES.map((p, i) => {
              const state = launch.error && i === at ? "error" : i < at ? "done" : i === at ? "active" : "todo";
              return (
                <li key={p.id} className="flex items-center gap-3 text-[0.9rem]">
                  <span
                    className={`inline-flex size-6 shrink-0 items-center justify-center rounded-full border ${
                      state === "done"
                        ? "border-[var(--color-success)] bg-[var(--color-success)] text-[var(--color-bg)]"
                        : state === "active"
                          ? "border-[var(--color-primary)] text-[var(--color-primary)]"
                          : state === "error"
                            ? "border-[var(--color-danger)] text-[var(--color-danger)]"
                            : "border-[var(--color-border)] text-[var(--color-text-muted)]"
                    }`}
                  >
                    {state === "done" ? (
                      <Icon name="check" className="size-3.5" />
                    ) : state === "error" ? (
                      <Icon name="alert-triangle" className="size-3.5" />
                    ) : state === "active" ? (
                      <span className="size-2 rounded-full bg-[var(--color-primary-fill)] motion-safe:animate-pulse" />
                    ) : null}
                  </span>
                  <span className={state === "todo" ? "text-[var(--color-text-muted)]" : "text-[var(--color-text-primary)]"}>{p.label}</span>
                </li>
              );
            })}
          </ol>
          {launch.error ? (
            <div className="max-w-md">
              <p role="alert" className="text-[0.85rem] text-[var(--color-danger)]">
                {launch.error}
              </p>
              <div className="mt-3 flex justify-center gap-2">
                <button type="button" className={btnClass({ reveal: true })} onClick={onBack}>
                  Back to the plan
                </button>
                {launch.conversationId ? (
                  <button type="button" className={btnClass({ variant: "primary" })} onClick={onOpen}>
                    Open the chat
                  </button>
                ) : null}
              </div>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}

export default PatrolLaunchpad;
