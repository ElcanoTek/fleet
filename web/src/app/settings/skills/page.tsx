"use client";

import { useEffect, useRef, useState } from "react";

import {
  btnClass,
  CodeChip,
  ConnBadge,
  InlineConfirmButton,
  RevealButton,
  SETTINGS_INPUT,
} from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRows,
  CopyChip,
  DirSearch,
  SetSection,
} from "../ui/panels";
import { ClampText } from "../ui/atoms";

// Skills library (Settings → Skills, fleet-unified settings pass). Two panels
// on the design's .skill-row anatomy: "Your skills" — the personal skill
// builder (docs/SKILLS.md phase 2: DB-owned, materialized only into the
// author's own runs, with the active/disabled/proposed lifecycle) — and the
// "Workspace pack" — every Agent Skill loaded on this deployment (the
// operator's bundle skills plus fleet's built-in pack), searchable with a full
// SKILL.md read view. Skills are invoked from chat by starting a message with
// "/<skill-name>" (the composer autocompletes them), or picked up
// automatically when a task matches a skill's description — every row leads
// with a copy chip so the invocation is one click away.

type SkillEntry = {
  name: string;
  description: string;
  source: string; // "bundle" | "plugin" | "builtin"
  plugin?: string; // the Agent Plugin name when source is "plugin"
  // The skill's declared `allowed-tools` (from SKILL.md frontmatter), surfaced
  // for review. fleet does NOT enforce it — a skill's real limits are the
  // sandbox, MCP allowlist, and approval gate — so it reads as a declared
  // contract, not a permission grant. Absent for skills that don't declare it.
  declared_allowed_tools?: string[];
};

type SkillDetail = SkillEntry & { content: string };

// A skill the user authored in the builder (docs/SKILLS.md phase 2): DB-owned,
// materialized only into the author's own runs.
type UserSkill = {
  id: string;
  name: string;
  description: string;
  body: string;
  status: "active" | "disabled" | "proposed";
};

const EMPTY_DRAFT = { id: "", name: "", description: "", body: "" };

// skillMd composes the SKILL.md a personal skill materializes as (the same
// frontmatter + body shape /api/skills/{name} returns for pack skills), so
// "View" reads identically across both panels.
function skillMd(sk: { name: string; description: string; body: string }): string {
  return `---\nname: ${sk.name}\ndescription: ${sk.description}\n---\n\n${sk.body}`;
}

// The design's .skill-row anatomy, shared by both panels so personal and pack
// rows read identically (head chip line, description, expandable SKILL.md).
const SKILL_ROW =
  "grid gap-[0.45rem] border-b border-[var(--color-border-subtle)] px-[0.1rem] py-3 last:border-b-0";
const SKILL_HEAD = "flex flex-wrap items-center gap-[0.55rem]";
// The design's .skill-desc metrics, applied over ClampText's base (with
// per-utility importance — same-property utilities are order-fragile). The
// brief adds the 3-line clamp + expand the mock lacks.
const SKILL_DESC = "text-[0.76rem]! leading-[1.55]!";
// .code-block.skill-md — capped height so a long SKILL.md scrolls inside the
// row instead of stretching the page.
const SKILL_MD =
  "mt-[0.15rem] max-h-[23rem] overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.95rem] py-[0.8rem] font-[family-name:var(--font-code)] text-[0.74rem] leading-[1.75] text-[var(--color-text-secondary)]";

const LOADING = "m-0 py-3 text-center text-[0.79rem] text-[var(--color-text-muted)]";

export default function SkillsPage() {
  const [skills, setSkills] = useState<SkillEntry[] | null>(null);
  const [query, setQuery] = useState("");
  const [openSkill, setOpenSkill] = useState<string | null>(null);
  const [detail, setDetail] = useState<SkillDetail | null>(null);
  // Monotonic ticket for view(): each click takes a new number and a response
  // only lands if its ticket is still current, so clicking A then B can never
  // paint A's body under B's header when A's fetch finishes last.
  const viewSeq = useRef(0);
  const [error, setError] = useState<string | null>(null);
  const [mine, setMine] = useState<UserSkill[] | null>(null);
  // Which personal skill is expanded (by id — a personal skill may share a
  // name with a pack skill, so the two panels track expansion independently).
  const [openMine, setOpenMine] = useState<string | null>(null);
  // The builder form: id === "" is a new skill; otherwise an edit.
  const [draft, setDraft] = useState<typeof EMPTY_DRAFT | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let stale = false;
    fetch("/api/skills", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`Failed to load skills: ${res.status}`);
        return (await res.json()) as { skills: SkillEntry[] };
      })
      .then((data) => {
        if (!stale) setSkills(data.skills ?? []);
      })
      .catch((e: unknown) => {
        if (!stale) setError(e instanceof Error ? e.message : "Failed to load skills.");
      });
    fetch("/api/user-skills", { cache: "no-store" })
      .then(async (res) => (res.ok ? ((await res.json()) as { skills: UserSkill[] }) : null))
      .then((data) => {
        if (!stale && data) setMine(data.skills ?? []);
      })
      .catch(() => {});
    return () => {
      stale = true;
    };
  }, []);

  const reloadMine = () =>
    fetch("/api/user-skills", { cache: "no-store" })
      .then(async (res) => (res.ok ? ((await res.json()) as { skills: UserSkill[] }) : null))
      .then((data) => {
        if (data) setMine(data.skills ?? []);
      })
      .catch(() => {});

  const saveDraft = () => {
    if (!draft) return;
    setBusy(true);
    setError(null);
    const isNew = draft.id === "";
    fetch(isNew ? "/api/user-skills" : `/api/user-skills/${encodeURIComponent(draft.id)}`, {
      method: isNew ? "POST" : "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: draft.name.trim(),
        description: draft.description.trim(),
        body: draft.body,
        // An edit must not change the lifecycle state — resend the current one.
        ...(isNew ? {} : { status: mine?.find((m) => m.id === draft.id)?.status ?? "active" }),
      }),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Save failed: ${res.status}`);
        setDraft(null);
        return reloadMine();
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : "Save failed."))
      .finally(() => setBusy(false));
  };

  const toggleMine = (sk: UserSkill) => {
    setBusy(true);
    setError(null);
    fetch(`/api/user-skills/${encodeURIComponent(sk.id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: sk.name,
        description: sk.description,
        body: sk.body,
        status: sk.status === "active" ? "disabled" : "active",
      }),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Update failed: ${res.status}`);
        return reloadMine();
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : "Update failed."))
      .finally(() => setBusy(false));
  };

  // approveMine flips an agent-proposed skill to active. Approval is the same
  // PUT the editor uses, so the server-side validation path stays identical.
  const approveMine = (sk: UserSkill) => {
    setBusy(true);
    setError(null);
    fetch(`/api/user-skills/${encodeURIComponent(sk.id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: sk.name,
        description: sk.description,
        body: sk.body,
        status: "active",
      }),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || "Approve failed.");
        return reloadMine();
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : "Approve failed."))
      .finally(() => setBusy(false));
  };

  // Deletion confirms inline (the two-click InlineConfirmButton) instead of a
  // native window.confirm dialog — the destructive intent stays in the row.
  const deleteMine = (sk: UserSkill) => {
    setBusy(true);
    setError(null);
    fetch(`/api/user-skills/${encodeURIComponent(sk.id)}`, { method: "DELETE" })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Delete failed: ${res.status}`);
        }
        return reloadMine();
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : "Delete failed."))
      .finally(() => setBusy(false));
  };

  const view = (name: string) => {
    const seq = ++viewSeq.current;
    if (openSkill === name) {
      setOpenSkill(null);
      setDetail(null);
      return;
    }
    setOpenSkill(name);
    setDetail(null);
    fetch(`/api/skills/${encodeURIComponent(name)}`, { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`Failed to load skill: ${res.status}`);
        return (await res.json()) as SkillDetail;
      })
      .then((d) => {
        if (seq === viewSeq.current) setDetail(d);
      })
      .catch((e: unknown) => {
        // A superseded request's failure is about a skill no longer open.
        if (seq !== viewSeq.current) return;
        setError(e instanceof Error ? e.message : "Failed to load skill.");
      });
  };

  const q = query.trim().toLowerCase();
  const filtered = (skills ?? []).filter(
    (s) =>
      !q || s.name.includes(q) || s.description.toLowerCase().includes(q) || s.source.includes(q),
  );

  return (
    <SetSection
      title="Skills"
      intro={
        <>
          Packaged, on-demand capabilities the agent can pick up when a task matches — your
          workspace’s own skills plus fleet’s built-in pack. Invoke one explicitly by starting a
          chat message with <CodeChip>/skill-name</CodeChip> (click a name below to copy it).
        </>
      }
    >
      {error ? <p className="mb-4 text-[0.78rem] text-[var(--color-danger)]">{error}</p> : null}

      <ConnPanel>
        <ConnPanelHead title="Your skills">
          <RevealButton
            open={draft !== null}
            closedLabel="New skill"
            disabled={busy}
            onClick={() => setDraft(draft ? null : { ...EMPTY_DRAFT })}
          />
        </ConnPanelHead>
        <ConnPanelSub>
          Skills you write here are yours alone — they load into your chats (invoke with{" "}
          <CodeChip>/name</CodeChip>) and never affect other users. Ask your operator to copy a
          proven one into the workspace bundle to share it.
        </ConnPanelSub>

        {draft ? (
          // .skill-create — the builder form doubles as the editor (draft.id
          // distinguishes); a bottom rule separates it from the rows below.
          <div className="mb-3 grid gap-[0.75rem] border-b border-[var(--color-border-subtle)] pb-4">
            <ConnField label="Name (lowercase-kebab, e.g. deal-check)">
              <input
                className={SETTINGS_INPUT}
                placeholder="my-skill"
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
            </ConnField>
            <ConnField label="Description — one line: what it does and when to use it (this is how the agent decides the skill applies)">
              <input
                className={SETTINGS_INPUT}
                placeholder="Verify a deal sheet before it goes to a client — use when asked to review or send a deal sheet."
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </ConnField>
            <ConnField label="Instructions (markdown — concrete steps the agent follows)">
              <textarea
                className={`${SETTINGS_INPUT} min-h-[9rem]! resize-y pt-[0.55rem]! font-[family-name:var(--font-code)] text-[0.76rem]! leading-[1.6]`}
                placeholder={"1. Read the attached sheet.\n2. Check…"}
                value={draft.body}
                onChange={(e) => setDraft({ ...draft, body: e.target.value })}
              />
            </ConnField>
            {/* The design tightens .conn-form-actions' top margin inside
                .skill-create (the grid gap already provides the spacing). */}
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
                disabled={busy || !draft.name.trim()}
                onClick={saveDraft}
              >
                {draft.id === "" ? "Create skill" : "Save changes"}
              </button>
            </div>
          </div>
        ) : null}

        {mine === null ? (
          <p className={LOADING}>Loading…</p>
        ) : mine.length === 0 && !draft ? (
          <ConnEmpty>
            No skills yet — capture a workflow you repeat and the agent will pick it up whenever it
            fits.
          </ConnEmpty>
        ) : (
          <ConnRows>
            {mine.map((sk) => (
              <div key={sk.id} data-testid={`skill-row-${sk.name}`} className={SKILL_ROW}>
                <div className={SKILL_HEAD}>
                  <CopyChip name={sk.name} />
                  <ConnBadge
                    variant={
                      sk.status === "active"
                        ? "success"
                        : sk.status === "proposed"
                          ? "warn"
                          : "neutral"
                    }
                  >
                    {sk.status === "active"
                      ? "Active"
                      : sk.status === "proposed"
                        ? "Proposed by agent"
                        : "Disabled"}
                  </ConnBadge>
                  <span className="ml-auto inline-flex gap-[0.35rem]">
                    {sk.status === "proposed" ? (
                      <button
                        type="button"
                        className={btnClass({ sm: true, reveal: true })}
                        disabled={busy}
                        onClick={() => approveMine(sk)}
                      >
                        Approve
                      </button>
                    ) : null}
                    <button
                      type="button"
                      className={btnClass({ sm: true, reveal: true })}
                      disabled={busy}
                      onClick={() =>
                        setDraft({
                          id: sk.id,
                          name: sk.name,
                          description: sk.description,
                          body: sk.body,
                        })
                      }
                    >
                      Edit
                    </button>
                    {sk.status !== "proposed" ? (
                      <button
                        type="button"
                        className={btnClass({ sm: true, reveal: true })}
                        disabled={busy}
                        onClick={() => toggleMine(sk)}
                      >
                        {sk.status === "active" ? "Disable" : "Enable"}
                      </button>
                    ) : null}
                    <button
                      type="button"
                      className={btnClass({ sm: true, reveal: true })}
                      aria-expanded={openMine === sk.id}
                      onClick={() => setOpenMine(openMine === sk.id ? null : sk.id)}
                    >
                      {openMine === sk.id ? "Hide" : "View"}
                    </button>
                    <InlineConfirmButton
                      label="Delete"
                      onConfirm={() => deleteMine(sk)}
                      disabled={busy}
                    />
                  </span>
                </div>
                {sk.description ? <ClampText text={sk.description} className={SKILL_DESC} /> : null}
                {openMine === sk.id ? <pre className={SKILL_MD}>{skillMd(sk)}</pre> : null}
              </div>
            ))}
          </ConnRows>
        )}
      </ConnPanel>

      <ConnPanel>
        <ConnPanelHead title="Workspace pack" />
        <ConnPanelSub>
          Shared with everyone in the workspace — your operator’s skills plus fleet’s built-in
          pack.
        </ConnPanelSub>
        <DirSearch
          className="mb-[0.35rem]"
          value={query}
          onChange={setQuery}
          placeholder={`Search ${skills?.length ?? 0} skills…`}
          label="Search workspace skills"
        />
        {skills === null ? (
          <p className={LOADING}>Loading…</p>
        ) : filtered.length === 0 ? (
          q ? (
            <ConnEmpty>No skills match “{query}”.</ConnEmpty>
          ) : (
            <ConnEmpty>No skills are installed on this deployment.</ConnEmpty>
          )
        ) : (
          <ConnRows>
            {filtered.map((s) => (
              <div key={s.name} data-testid={`skill-row-${s.name}`} className={SKILL_ROW}>
                <div className={SKILL_HEAD}>
                  <CopyChip name={s.name} />
                  <ConnBadge variant={s.source === "bundle" ? "success" : "neutral"}>
                    {s.source === "bundle"
                      ? "Workspace"
                      : s.source === "plugin"
                        ? `Plugin${s.plugin ? `: ${s.plugin}` : ""}`
                        : "Built-in"}
                  </ConnBadge>
                  <button
                    type="button"
                    className={`ml-auto flex-none ${btnClass({ sm: true, reveal: true })}`}
                    aria-expanded={openSkill === s.name}
                    onClick={() => view(s.name)}
                  >
                    {openSkill === s.name ? "Hide" : "View"}
                  </button>
                </div>
                <ClampText text={s.description} className={SKILL_DESC} />
                {s.declared_allowed_tools && s.declared_allowed_tools.length > 0 ? (
                  <p className="mb-0 mt-[0.15rem] text-[0.6875rem] text-[var(--color-text-muted)]">
                    <span className="uppercase tracking-wide">Declared tools</span>{" "}
                    {s.declared_allowed_tools.join(", ")}{" "}
                    <span title="Declared for review only — fleet does not enforce a skill's allowed-tools. The real limits are the sandbox, MCP allowlist, and approval gate.">
                      (advisory)
                    </span>
                  </p>
                ) : null}
                {openSkill === s.name ? (
                  // The name check is a second guard on top of view()'s ticket:
                  // a body only ever renders under its own header.
                  detail && detail.name === s.name ? (
                    <pre className={SKILL_MD}>{detail.content}</pre>
                  ) : (
                    <p className="m-0 text-[0.74rem] text-[var(--color-text-muted)]">Loading…</p>
                  )
                ) : null}
              </div>
            ))}
          </ConnRows>
        )}
      </ConnPanel>
    </SetSection>
  );
}
