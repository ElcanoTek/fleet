"use client";

import { useEffect, useRef, useState } from "react";

import { navigate } from "@/app/lib/navigation";
import { currentDefaultModel } from "@/app/lib/modelAliases";
import { btnClass, ConnBadge, InlineConfirmButton, RevealButton, SETTINGS_INPUT } from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRows,
  SetSection,
} from "../ui/panels";
import { ClampText } from "../ui/atoms";
import {
  browserTimeZone,
  CADENCE_PRESETS,
  cadenceLabel,
  EMPTY_PERSONA,
  PERSONA_TEMPLATES,
  slugify,
  twinBuilderMessage,
  type PersonaAutonomy,
  type PersonaDraft,
  type PersonaPatrol,
} from "./personaTemplates";

// Personas (Settings → Personas, docs/USER-PERSONAS.md). A persona is how the
// assistant thinks, writes and patrols in chats you start with it. Three
// panels: the digital-twin builder (hands a ready-written request to the chat
// assistant, which drafts a persona with persona_studio), "Your personas" (the
// editor, review queue for drafts, and one-click Start chat / Start patrol),
// and the workspace personas yours can build on. Yours are private: only your
// own chats can use them.

type UserPersona = {
  id: string;
  name: string;
  ref: string;
  display_name: string;
  description: string;
  instructions: string;
  base_persona: string;
  kind: "assistant" | "twin";
  autonomy: PersonaAutonomy;
  patrols: PersonaPatrol[];
  status: "active" | "proposed";
  replaces_name?: string;
};

type ListResponse = {
  personas: UserPersona[];
  workspace_personas: string[];
  default: string;
};

const ROW =
  "grid gap-[0.45rem] border-b border-[var(--color-border-subtle)] px-[0.1rem] py-3 last:border-b-0";
const ROW_HEAD = "flex flex-wrap items-center gap-[0.55rem]";
const DESC = "text-[0.76rem]! leading-[1.55]!";
const MD =
  "mt-[0.15rem] max-h-[23rem] overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.95rem] py-[0.8rem] font-[family-name:var(--font-code)] text-[0.74rem] leading-[1.75] text-[var(--color-text-secondary)]";
const LOADING = "m-0 py-3 text-center text-[0.79rem] text-[var(--color-text-muted)]";
const HINT = "m-0 text-[0.72rem] text-[var(--color-text-muted)]";
const CHOICE =
  "grid cursor-pointer grid-cols-[auto_1fr] items-start gap-x-2 gap-y-[0.1rem] rounded-[var(--radius-md)] border border-[var(--color-border)] px-[0.7rem] py-[0.55rem] text-[0.78rem] text-[var(--color-text-secondary)] has-[:checked]:border-[var(--color-accent)] has-[:checked]:bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)]";

function titleCase(p: string): string {
  return p.charAt(0).toUpperCase() + p.slice(1);
}

async function failText(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  return text || `${fallback} (${res.status})`;
}

/** Choice renders one radio card of a two-way setting. */
function Choice({
  name,
  checked,
  onChange,
  title,
  hint,
}: {
  name: string;
  checked: boolean;
  onChange: () => void;
  title: string;
  hint: string;
}) {
  return (
    <label className={CHOICE}>
      <input type="radio" name={name} checked={checked} onChange={onChange} className="row-span-2 mt-[0.2rem]" />
      <span className="font-medium text-[var(--color-text-primary)]">{title}</span>
      <span className="col-start-2 text-[0.72rem] text-[var(--color-text-muted)]">{hint}</span>
    </label>
  );
}

export default function PersonasPage() {
  const [personas, setPersonas] = useState<UserPersona[] | null>(null);
  const [workspace, setWorkspace] = useState<string[]>([]);
  const [defaultPersona, setDefaultPersona] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState<PersonaDraft | null>(null);
  // The slug follows the display name until the user edits it by hand.
  const [slugTouched, setSlugTouched] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  // ?review=<id> (the persona_studio tool's link) highlights that draft and
  // opens it.
  const [reviewId] = useState<string | null>(() =>
    typeof window === "undefined" ? null : new URLSearchParams(window.location.search).get("review"),
  );
  const [open, setOpen] = useState<string | null>(reviewId);
  const editorRef = useRef<HTMLDivElement | null>(null);

  // Twin builder
  const [about, setAbout] = useState("");
  const [useHistory, setUseHistory] = useState(true);
  const [twinAutonomy, setTwinAutonomy] = useState<PersonaAutonomy>("advise");

  const reload = () =>
    fetch("/api/user-personas", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(await failText(res, "Failed to load your personas"));
        return (await res.json()) as ListResponse;
      })
      .then((data) => {
        setPersonas(data.personas ?? []);
        setWorkspace(data.workspace_personas ?? []);
        setDefaultPersona(data.default ?? "");
        setError(null);
      })
      .catch((e: unknown) => {
        setPersonas((prev) => prev ?? []);
        setError(e instanceof Error ? e.message : "Failed to load your personas.");
      });

  useEffect(() => {
    void reload();
  }, []);

  // Bring the draft a ?review= link names into view once the list is in.
  const reviewFound = Boolean(reviewId && personas?.some((p) => p.id === reviewId));
  useEffect(() => {
    if (reviewFound) {
      document.getElementById(`persona-${reviewId}`)?.scrollIntoView?.({ block: "center" });
    }
  }, [reviewFound, reviewId]);

  const openEditor = (d: PersonaDraft) => {
    setDraft(d);
    setSlugTouched(d.id !== "");
    setNotice(null);
    setTimeout(() => editorRef.current?.scrollIntoView?.({ behavior: "smooth", block: "nearest" }), 0);
  };

  const run = async (label: string, fn: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await fn();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : `${label} failed.`);
    } finally {
      setBusy(false);
    }
  };

  const save = () =>
    run("Save", async () => {
      if (!draft) return;
      const isNew = draft.id === "";
      const res = await fetch(isNew ? "/api/user-personas" : `/api/user-personas/${encodeURIComponent(draft.id)}`, {
        method: isNew ? "POST" : "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: draft.name.trim(),
          display_name: draft.display_name.trim(),
          description: draft.description.trim(),
          instructions: draft.instructions,
          base_persona: draft.base_persona,
          kind: draft.kind,
          autonomy: draft.autonomy,
          patrols: draft.patrols,
        }),
      });
      if (!res.ok) throw new Error(await failText(res, "Save failed"));
      setNotice(isNew ? `Created “${draft.display_name.trim()}”. Pick it from the persona menu in a new chat.` : "Saved.");
      setDraft(null);
      await reload();
    });

  const approve = (p: UserPersona) =>
    run("Approve", async () => {
      const res = await fetch(`/api/user-personas/${encodeURIComponent(p.id)}/approve`, { method: "POST" });
      if (!res.ok) throw new Error(await failText(res, "Approve failed"));
      setNotice(p.replaces_name ? `Applied the edit to “${p.replaces_name}”.` : `“${p.display_name}” is ready to use.`);
      await reload();
    });

  const remove = (p: UserPersona) =>
    run("Delete", async () => {
      const res = await fetch(`/api/user-personas/${encodeURIComponent(p.id)}`, { method: "DELETE" });
      if (!res.ok && res.status !== 204) throw new Error(await failText(res, "Delete failed"));
      await reload();
    });

  const startChat = (ref: string) => navigate(`/chat?persona=${encodeURIComponent(ref)}`);

  // startPatrol makes a new chat on the persona, attaches the suggested patrol
  // to it and runs the first check right away, then opens the chat. A persona
  // set to act on your behalf turns the chat's auto-approve on; one that
  // advises leaves approval cards in place.
  const startPatrol = (p: UserPersona, patrol: PersonaPatrol) =>
    run("Start patrol", async () => {
      const convRes = await fetch("/api/conversations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ persona: p.ref, title: patrol.title, model: currentDefaultModel() }),
      });
      if (!convRes.ok) throw new Error(await failText(convRes, "Could not create the patrol chat"));
      const conv = (await convRes.json()) as { id: string };
      const base = `/api/conversations/${encodeURIComponent(conv.id)}/schedules`;
      const schedRes = await fetch(base, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title: patrol.title,
          prompt: patrol.prompt,
          cron: patrol.cron,
          timezone: patrol.timezone || "UTC",
          auto_approve: p.autonomy === "act",
        }),
      });
      if (!schedRes.ok) throw new Error(await failText(schedRes, "Could not start the patrol"));
      const sched = (await schedRes.json()) as { schedule?: { id: string } };
      if (sched.schedule?.id) {
        // The first check runs now so the chat shows the persona at work;
        // a refusal here (e.g. a full queue) still leaves the patrol armed.
        await fetch(`${base}/${encodeURIComponent(sched.schedule.id)}/run-now`, { method: "POST" }).catch(
          () => undefined,
        );
      }
      navigate(`/chat?c=${encodeURIComponent(conv.id)}`);
    });

  const buildTwin = () =>
    navigate(`/chat?draft=${encodeURIComponent(twinBuilderMessage({ about, useHistory, autonomy: twinAutonomy }))}`);

  const baseLabel = (base: string) => titleCase(base || defaultPersona || "default");

  const setPatrol = (i: number, patch: Partial<PersonaPatrol>) => {
    if (!draft) return;
    setDraft({ ...draft, patrols: draft.patrols.map((pt, j) => (j === i ? { ...pt, ...patch } : pt)) });
  };

  const pending = (personas ?? []).filter((p) => p.status === "proposed");

  return (
    <SetSection
      title="Personas"
      intro={
        <>
          A persona shapes how the assistant thinks, writes and patrols in the chats you start with
          it. Build a digital twin that works the way you do, or a specialist for one job. Yours are
          private — only your own chats can use them. Pick one from the persona menu in a new chat,
          or start a patrol straight from its card.
        </>
      }
    >
      {error ? (
        <p role="alert" className="mb-4 text-[0.78rem] text-[var(--color-danger)]">
          {error}
        </p>
      ) : null}
      {notice ? (
        <p role="status" className="mb-4 text-[0.78rem] text-[var(--color-text-secondary)]">
          {notice}
        </p>
      ) : null}

      <ConnPanel>
        <ConnPanelHead title="Build your digital twin" />
        <ConnPanelSub>
          Tell the assistant about yourself and it drafts a persona that thinks, prioritizes and
          writes like you — with suggested patrols for the checks you repeat. Nothing is saved until
          you approve the draft here.
        </ConnPanelSub>
        <div className="grid gap-[0.75rem]">
          <ConnField label="About you — your role, what you watch, how you decide, how you like things written">
            <textarea
              id="twinAbout"
              className={`${SETTINGS_INPUT} min-h-[5.5rem]! resize-y pt-[0.55rem]! text-[0.85rem]!`}
              placeholder="I run programmatic deals for retail clients. I check pacing and margin every morning, flag anything under 80% of target, and like short bullet summaries with the number first."
              value={about}
              onChange={(e) => setAbout(e.target.value)}
            />
          </ConnField>
          <label className="flex cursor-pointer items-center gap-2 text-[0.78rem] text-[var(--color-text-secondary)]">
            <input type="checkbox" checked={useHistory} onChange={(e) => setUseHistory(e.target.checked)} />
            <span>
              Learn from my recent chats — the assistant reads only the messages you typed (not its
              replies, and never lockdown chats)
            </span>
          </label>
          <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label="What your twin may do">
            <Choice
              name="twinAutonomy"
              checked={twinAutonomy === "advise"}
              onChange={() => setTwinAutonomy("advise")}
              title="Suggest actions"
              hint="Analyzes and recommends; you approve anything it does."
            />
            <Choice
              name="twinAutonomy"
              checked={twinAutonomy === "act"}
              onChange={() => setTwinAutonomy("act")}
              title="Act on my behalf"
              hint="Handles routine actions itself; patrols run with auto-approve on."
            />
          </div>
          <div className="flex flex-wrap justify-end gap-2">
            <button
              type="button"
              className={btnClass({ sm: true, reveal: true })}
              disabled={busy}
              onClick={() => {
                const t = PERSONA_TEMPLATES[0].draft();
                openEditor({ ...EMPTY_PERSONA, ...t, name: slugify(t.display_name ?? ""), autonomy: twinAutonomy });
              }}
            >
              Write it myself
            </button>
            <button type="button" className={btnClass({ variant: "primary" })} disabled={busy} onClick={buildTwin}>
              Build with the assistant
            </button>
          </div>
          <p className={HINT}>
            Opens a new chat with your request ready to send. The assistant asks a few questions,
            then stages a draft persona below for you to approve.
          </p>
        </div>
      </ConnPanel>

      <ConnPanel>
        <ConnPanelHead title="Your personas">
          <RevealButton
            open={draft !== null}
            closedLabel="New persona"
            disabled={busy}
            testId="new-persona"
            onClick={() => (draft ? setDraft(null) : openEditor({ ...EMPTY_PERSONA }))}
          />
        </ConnPanelHead>
        <ConnPanelSub>
          Drafts the assistant proposes wait here for your review. Editing a persona changes every
          chat that uses it from its next turn.
        </ConnPanelSub>

        {draft ? (
          <div
            ref={editorRef}
            data-testid="persona-editor"
            className="mb-3 grid gap-[0.75rem] border-b border-[var(--color-border-subtle)] pb-4"
          >
            {draft.id === "" ? (
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-[0.72rem] font-medium text-[var(--color-text-secondary)]">Start from</span>
                {PERSONA_TEMPLATES.map((t) => (
                  <button
                    key={t.id}
                    type="button"
                    title={t.hint}
                    className={btnClass({ sm: true, reveal: true })}
                    onClick={() => {
                      const d = t.draft();
                      setDraft({ ...EMPTY_PERSONA, ...d, name: slugify(d.display_name ?? "") });
                      setSlugTouched(false);
                    }}
                  >
                    {t.label}
                  </button>
                ))}
              </div>
            ) : null}
            <div className="flex flex-wrap gap-[0.75rem]">
              <ConnField label="Display name" grow>
                <input
                  id="personaDisplayName"
                  className={SETTINGS_INPUT}
                  placeholder="My Twin"
                  value={draft.display_name}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      display_name: e.target.value,
                      name: draft.id === "" && !slugTouched ? slugify(e.target.value) : draft.name,
                    })
                  }
                />
              </ConnField>
              <ConnField label={draft.id === "" ? "Short name (lowercase-kebab)" : "Short name (fixed — chats refer to it)"}>
                <input
                  id="personaName"
                  className={SETTINGS_INPUT}
                  placeholder="my-twin"
                  value={draft.name}
                  readOnly={draft.id !== ""}
                  onChange={(e) => {
                    setSlugTouched(true);
                    setDraft({ ...draft, name: e.target.value });
                  }}
                />
              </ConnField>
            </div>
            <ConnField label="One line — what it's for">
              <input
                id="personaDescription"
                className={SETTINGS_INPUT}
                placeholder="Watches my deals and drafts updates in my voice."
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </ConnField>
            <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label="Kind">
              <Choice
                name="personaKind"
                checked={draft.kind === "twin"}
                onChange={() => setDraft({ ...draft, kind: "twin" })}
                title="Digital twin"
                hint="Thinks, prioritizes and writes the way you would."
              />
              <Choice
                name="personaKind"
                checked={draft.kind === "assistant"}
                onChange={() => setDraft({ ...draft, kind: "assistant" })}
                title="Specialist assistant"
                hint="A helper set up for one kind of work."
              />
            </div>
            <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label="What it may do">
              <Choice
                name="personaAutonomy"
                checked={draft.autonomy === "advise"}
                onChange={() => setDraft({ ...draft, autonomy: "advise" })}
                title="Suggest actions"
                hint="Analyzes and recommends; you approve anything it does."
              />
              <Choice
                name="personaAutonomy"
                checked={draft.autonomy === "act"}
                onChange={() => setDraft({ ...draft, autonomy: "act" })}
                title="Act on my behalf"
                hint="Handles routine actions itself; patrols run with auto-approve on."
              />
            </div>
            <ConnField label="Builds on (its expertise and tool access carry over)">
              <select
                id="personaBase"
                className={SETTINGS_INPUT}
                value={draft.base_persona}
                onChange={(e) => setDraft({ ...draft, base_persona: e.target.value })}
              >
                <option value="">Workspace default ({baseLabel("")})</option>
                {workspace.map((w) => (
                  <option key={w} value={w}>
                    {titleCase(w)}
                  </option>
                ))}
              </select>
            </ConnField>
            <ConnField label="Instructions (markdown — role, priorities, decision rules, voice, what never to do)">
              <textarea
                id="personaInstructions"
                className={`${SETTINGS_INPUT} min-h-[11rem]! resize-y pt-[0.55rem]! font-[family-name:var(--font-code)] text-[0.76rem]! leading-[1.6]`}
                placeholder={"## Who I am\n- …\n\n## How I decide\n- …"}
                value={draft.instructions}
                onChange={(e) => setDraft({ ...draft, instructions: e.target.value })}
              />
            </ConnField>

            <fieldset className="grid gap-[0.6rem] rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] px-[0.8rem] pb-[0.8rem] pt-[0.4rem]">
              <legend className="px-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                Suggested patrols
              </legend>
              <p className={HINT}>
                Checks this persona can run on a schedule. Nothing runs until you press Start patrol on
                the persona&apos;s card.
              </p>
              {draft.patrols.map((pt, i) => {
                const preset = CADENCE_PRESETS.some((c) => c.cron === pt.cron) ? pt.cron : "custom";
                return (
                  <div
                    key={i}
                    data-testid={`patrol-row-${i}`}
                    className="grid gap-[0.5rem] border-t border-[var(--color-border-subtle)] pt-[0.6rem] first-of-type:border-t-0"
                  >
                    <div className="flex flex-wrap gap-[0.6rem]">
                      <ConnField label="Title" grow>
                        <input
                          className={SETTINGS_INPUT}
                          value={pt.title}
                          placeholder="Morning pacing sweep"
                          onChange={(e) => setPatrol(i, { title: e.target.value })}
                        />
                      </ConnField>
                      <ConnField label="How often">
                        <select
                          className={SETTINGS_INPUT}
                          value={preset}
                          onChange={(e) => setPatrol(i, { cron: e.target.value === "custom" ? "" : e.target.value })}
                        >
                          {CADENCE_PRESETS.map((c) => (
                            <option key={c.cron} value={c.cron}>
                              {c.label}
                            </option>
                          ))}
                          <option value="custom">Custom (cron)</option>
                        </select>
                      </ConnField>
                      {preset === "custom" ? (
                        <ConnField label="Cron (5 fields)">
                          <input
                            className={SETTINGS_INPUT}
                            value={pt.cron}
                            placeholder="30 7 * * 1-5"
                            onChange={(e) => setPatrol(i, { cron: e.target.value })}
                          />
                        </ConnField>
                      ) : null}
                      <ConnField label="Time zone">
                        <input
                          className={SETTINGS_INPUT}
                          value={pt.timezone ?? ""}
                          placeholder="UTC"
                          onChange={(e) => setPatrol(i, { timezone: e.target.value })}
                        />
                      </ConnField>
                    </div>
                    <ConnField label="What to do each time">
                      <textarea
                        className={`${SETTINGS_INPUT} min-h-[4rem]! resize-y pt-[0.55rem]! text-[0.82rem]!`}
                        value={pt.prompt}
                        placeholder="Check pacing on my top deals and flag anything under 80% of target."
                        onChange={(e) => setPatrol(i, { prompt: e.target.value })}
                      />
                    </ConnField>
                    <div className="flex justify-end">
                      <button
                        type="button"
                        className={btnClass({ sm: true, danger: true })}
                        onClick={() => setDraft({ ...draft, patrols: draft.patrols.filter((_, j) => j !== i) })}
                      >
                        Remove patrol
                      </button>
                    </div>
                  </div>
                );
              })}
              {draft.patrols.length < 8 ? (
                <div>
                  <button
                    type="button"
                    className={btnClass({ sm: true, reveal: true })}
                    onClick={() =>
                      setDraft({
                        ...draft,
                        patrols: [
                          ...draft.patrols,
                          { title: "", cron: "0 9 * * 1-5", timezone: browserTimeZone(), prompt: "" },
                        ],
                      })
                    }
                  >
                    Add a patrol
                  </button>
                </div>
              ) : null}
            </fieldset>

            <div className="mt-[0.1rem] flex justify-end gap-2">
              <button
                type="button"
                className={btnClass({ sm: true, reveal: true })}
                disabled={busy}
                onClick={() => setDraft(null)}
              >
                Cancel
              </button>
              <button
                type="button"
                className={btnClass({ variant: "primary" })}
                disabled={busy || !draft.name.trim() || !draft.instructions.trim()}
                onClick={() => void save()}
              >
                {draft.id === "" ? "Create persona" : "Save changes"}
              </button>
            </div>
          </div>
        ) : null}

        {pending.length > 0 ? (
          <p className="mb-2 text-[0.76rem] text-[var(--color-text-secondary)]">
            {pending.length === 1 ? "1 draft is" : `${pending.length} drafts are`} waiting for your review.
          </p>
        ) : null}

        {personas === null ? (
          <p className={LOADING}>Loading…</p>
        ) : personas.length === 0 && !draft ? (
          <ConnEmpty>
            No personas yet — build your digital twin above, or start from a template with New
            persona.
          </ConnEmpty>
        ) : (
          <ConnRows>
            {personas.map((p) => {
              const proposed = p.status === "proposed";
              return (
                <div
                  key={p.id}
                  id={`persona-${p.id}`}
                  data-testid={`persona-row-${p.name}`}
                  className={`${ROW} ${p.id === reviewId ? "rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-accent)_8%,transparent)]" : ""}`}
                >
                  <div className={ROW_HEAD}>
                    <span className="text-[0.88rem] font-semibold text-[var(--color-text-primary)]">
                      {p.display_name}
                    </span>
                    <ConnBadge variant={p.kind === "twin" ? "overridden" : "neutral"}>
                      {p.kind === "twin" ? "Digital twin" : "Assistant"}
                    </ConnBadge>
                    <ConnBadge variant="neutral">{p.autonomy === "act" ? "Acts for you" : "Suggests actions"}</ConnBadge>
                    {proposed ? (
                      <ConnBadge variant="warn">
                        {p.replaces_name ? `Proposed edit to ${p.replaces_name}` : "Proposed — review it"}
                      </ConnBadge>
                    ) : null}
                    <span className="ml-auto inline-flex flex-wrap gap-[0.35rem]">
                      {proposed ? (
                        <button
                          type="button"
                          className={btnClass({ sm: true, variant: "primary" })}
                          disabled={busy}
                          onClick={() => void approve(p)}
                        >
                          Approve
                        </button>
                      ) : (
                        <>
                          <button
                            type="button"
                            className={btnClass({ sm: true, reveal: true })}
                            disabled={busy}
                            onClick={() => startChat(p.ref)}
                          >
                            Start chat
                          </button>
                          <button
                            type="button"
                            className={btnClass({ sm: true, reveal: true })}
                            disabled={busy}
                            onClick={() =>
                              openEditor({
                                id: p.id,
                                name: p.name,
                                display_name: p.display_name,
                                description: p.description,
                                instructions: p.instructions,
                                base_persona: p.base_persona,
                                kind: p.kind,
                                autonomy: p.autonomy,
                                patrols: p.patrols ?? [],
                              })
                            }
                          >
                            Edit
                          </button>
                        </>
                      )}
                      <button
                        type="button"
                        className={btnClass({ sm: true, reveal: true })}
                        aria-expanded={open === p.id}
                        onClick={() => setOpen(open === p.id ? null : p.id)}
                      >
                        {open === p.id ? "Hide" : "View"}
                      </button>
                      <InlineConfirmButton label={proposed ? "Discard" : "Delete"} onConfirm={() => void remove(p)} disabled={busy} />
                    </span>
                  </div>
                  {p.description ? <ClampText text={p.description} className={DESC} /> : null}
                  <p className={HINT}>Builds on {baseLabel(p.base_persona)}</p>
                  {open === p.id ? <pre className={MD}>{p.instructions}</pre> : null}
                  {p.patrols && p.patrols.length > 0 ? (
                    <ul className="m-0 grid list-none gap-[0.35rem] p-0" aria-label={`Suggested patrols for ${p.display_name}`}>
                      {p.patrols.map((pt, i) => (
                        <li
                          key={i}
                          className="flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] px-[0.65rem] py-[0.45rem] text-[0.76rem]"
                        >
                          <span className="font-medium text-[var(--color-text-primary)]">{pt.title}</span>
                          <span className="text-[var(--color-text-muted)]">
                            {cadenceLabel(pt.cron)}
                            {pt.timezone && pt.timezone !== "UTC" ? ` · ${pt.timezone}` : pt.timezone === "UTC" ? " · UTC" : ""}
                          </span>
                          {!proposed ? (
                            <button
                              type="button"
                              className={`ml-auto ${btnClass({ sm: true, reveal: true })}`}
                              disabled={busy}
                              title="Opens a new chat with this persona, starts the patrol and runs the first check now"
                              onClick={() => void startPatrol(p, pt)}
                            >
                              Start patrol
                            </button>
                          ) : null}
                          {open === p.id ? (
                            <span className="basis-full text-[0.74rem] text-[var(--color-text-secondary)]">{pt.prompt}</span>
                          ) : null}
                        </li>
                      ))}
                    </ul>
                  ) : null}
                </div>
              );
            })}
          </ConnRows>
        )}
      </ConnPanel>

      <ConnPanel>
        <ConnPanelHead title="Workspace personas" />
        <ConnPanelSub>
          Set up by your workspace for everyone. Customize one to start your own persona from it —
          its expertise and tool access carry over.
        </ConnPanelSub>
        {workspace.length === 0 ? (
          <ConnEmpty>No workspace personas are installed.</ConnEmpty>
        ) : (
          <ConnRows>
            {workspace.map((w) => (
              <div key={w} data-testid={`workspace-persona-${w}`} className={ROW}>
                <div className={ROW_HEAD}>
                  <span className="text-[0.85rem] font-medium text-[var(--color-text-primary)]">{titleCase(w)}</span>
                  {w === defaultPersona ? <ConnBadge variant="success">Default</ConnBadge> : null}
                  <span className="ml-auto inline-flex gap-[0.35rem]">
                    <button
                      type="button"
                      className={btnClass({ sm: true, reveal: true })}
                      disabled={busy}
                      onClick={() => startChat(w)}
                    >
                      Start chat
                    </button>
                    <button
                      type="button"
                      className={btnClass({ sm: true, reveal: true })}
                      disabled={busy}
                      onClick={() =>
                        openEditor({
                          ...EMPTY_PERSONA,
                          display_name: `My ${titleCase(w)}`,
                          name: slugify(`my ${w}`),
                          base_persona: w,
                          instructions: `Build on the ${titleCase(w)} persona, and:\n\n- …`,
                        })
                      }
                    >
                      Customize
                    </button>
                  </span>
                </div>
              </div>
            ))}
          </ConnRows>
        )}
      </ConnPanel>
    </SetSection>
  );
}
