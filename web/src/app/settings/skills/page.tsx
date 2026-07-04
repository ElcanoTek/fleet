"use client";

import { useEffect, useState } from "react";

import { StatusChip } from "@/app/shared/ui/StatusChip";
import { SettingsShell } from "../SettingsShell";

// Skills library (Settings → Skills). Browses every Agent Skill loaded on
// this deployment — the operator's bundle skills plus fleet's built-in pack —
// with search and a full SKILL.md read view. Skills are invoked from chat by
// starting a message with "/<skill-name>" (the composer autocompletes them),
// or picked up automatically when a task matches a skill's description.

type SkillEntry = {
  name: string;
  description: string;
  source: string; // "bundle" | "builtin"
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

export default function SkillsPage() {
  const [skills, setSkills] = useState<SkillEntry[] | null>(null);
  const [query, setQuery] = useState("");
  const [openSkill, setOpenSkill] = useState<string | null>(null);
  const [detail, setDetail] = useState<SkillDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [mine, setMine] = useState<UserSkill[] | null>(null);
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

  const deleteMine = (sk: UserSkill) => {
    if (!window.confirm(`Delete your skill "${sk.name}"? This cannot be undone.`)) return;
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
      .then(setDetail)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : "Failed to load skill."),
      );
  };

  const q = query.trim().toLowerCase();
  const filtered = (skills ?? []).filter(
    (s) =>
      !q ||
      s.name.includes(q) ||
      s.description.toLowerCase().includes(q) ||
      s.source.includes(q),
  );

  return (
    <SettingsShell
      title="Skills"
      description={
        <>
          Packaged, on-demand capabilities the agent can pick up when a task
          matches — your workspace&apos;s own skills plus fleet&apos;s built-in
          pack. Invoke one explicitly by starting a chat message with{" "}
          <code className="rounded bg-[var(--color-overlay-soft)] px-1 py-0.5 text-[0.8125rem]">
            /skill-name
          </code>
          .
        </>
      }
    >
      <>
        {error ? (
          <p className="mb-4 text-[0.8125rem] text-[var(--color-danger-soft)]">
            {error}
          </p>
        ) : null}

        <div className="mb-6 overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
          <div className="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2">
            <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              Your skills
            </span>
            <button
              type="button"
              onClick={() => setDraft({ ...EMPTY_DRAFT })}
              disabled={busy}
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
            >
              New skill
            </button>
          </div>
          <p className="px-4 pt-3 text-[0.75rem] text-[var(--color-text-muted)]">
            Skills you write here are yours alone — they load into your chats
            (invoke with /name) and never affect other users. Ask your operator
            to copy a proven one into the workspace bundle to share it.
          </p>
          {draft ? (
            <div className="border-b border-[var(--color-border-subtle)] px-4 py-3">
              <div className="grid gap-2">
                <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                  Name (lowercase-kebab, e.g. deal-check)
                  <input
                    value={draft.name}
                    onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                    disabled={draft.id !== "" && busy}
                    placeholder="my-skill"
                    className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                  />
                </label>
                <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                  Description — one line: what it does and when to use it (this
                  is how the agent decides the skill applies)
                  <input
                    value={draft.description}
                    onChange={(e) => setDraft({ ...draft, description: e.target.value })}
                    placeholder="Verify a deal sheet before it goes to a client — use when asked to review or send a deal sheet."
                    className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                  />
                </label>
                <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                  Instructions (markdown — concrete steps the agent follows)
                  <textarea
                    value={draft.body}
                    onChange={(e) => setDraft({ ...draft, body: e.target.value })}
                    rows={10}
                    placeholder={"1. Read the attached sheet.\n2. Check…"}
                    className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 font-mono text-[0.8125rem] leading-relaxed text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                  />
                </label>
                <div className="flex justify-end gap-2">
                  <button
                    type="button"
                    onClick={() => setDraft(null)}
                    disabled={busy}
                    className="rounded-full border border-[var(--color-border-subtle)] px-4 py-1.5 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                  >
                    Cancel
                  </button>
                  <button
                    type="button"
                    onClick={saveDraft}
                    disabled={busy || !draft.name.trim() || !draft.description.trim() || !draft.body.trim()}
                    className="rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                  >
                    {draft.id === "" ? "Create skill" : "Save changes"}
                  </button>
                </div>
              </div>
            </div>
          ) : null}
          {mine === null ? (
            <p className="px-4 py-4 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              Loading…
            </p>
          ) : mine.length === 0 && !draft ? (
            <p className="px-4 py-4 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              No skills yet — capture a workflow you repeat and the agent will
              pick it up whenever it fits.
            </p>
          ) : (
            <ul>
              {mine.map((sk) => (
                <li
                  key={sk.id}
                  className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium">{sk.name}</span>
                      <StatusChip
                        tone={
                          sk.status === "active"
                            ? "success"
                            : sk.status === "proposed"
                              ? "warning"
                              : "neutral"
                        }
                      >
                        {sk.status === "active"
                          ? "Active"
                          : sk.status === "proposed"
                            ? "Proposed by agent"
                            : "Disabled"}
                      </StatusChip>
                    </div>
                    <p className="mt-0.5 text-[0.8125rem] text-[var(--color-text-muted)]">
                      {sk.description}
                    </p>
                  </div>
                  <div className="flex items-center gap-2">
                    {sk.status === "proposed" ? (
                      <button
                        type="button"
                        onClick={() =>
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
                            .catch((e: unknown) =>
                              setError(e instanceof Error ? e.message : "Approve failed."),
                            )
                        }
                        disabled={busy}
                        className="rounded-full border border-[var(--color-success-strong)] px-3 py-1 text-[0.75rem] text-[var(--color-success-soft)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                      >
                        Approve
                      </button>
                    ) : null}
                    <button
                      type="button"
                      onClick={() => setDraft({ id: sk.id, name: sk.name, description: sk.description, body: sk.body })}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      Edit
                    </button>
                    {sk.status !== "proposed" ? (
                      <button
                        type="button"
                        onClick={() => toggleMine(sk)}
                        disabled={busy}
                        className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                      >
                        {sk.status === "active" ? "Disable" : "Enable"}
                      </button>
                    ) : null}
                    <button
                      type="button"
                      onClick={() => deleteMine(sk)}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      Delete
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className="overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
          <div className="border-b border-[var(--color-border)] px-4 py-3">
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={`Search ${skills?.length ?? 0} skills…`}
              className="w-full rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </div>
          {skills === null ? (
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              Loading…
            </p>
          ) : filtered.length === 0 ? (
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              No skills match.
            </p>
          ) : (
            <ul>
              {filtered.map((s) => (
                <li
                  key={s.name}
                  className="border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="flex min-w-0 items-center gap-2">
                      <span className="font-medium">{s.name}</span>
                      <StatusChip tone={s.source === "bundle" ? "success" : "neutral"}>
                        {s.source === "bundle" ? "Workspace" : "Built-in"}
                      </StatusChip>
                    </div>
                    <button
                      type="button"
                      onClick={() => view(s.name)}
                      className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)]"
                    >
                      {openSkill === s.name ? "Hide" : "View"}
                    </button>
                  </div>
                  <p className="mt-1 text-[0.8125rem] text-[var(--color-text-muted)]">
                    {s.description}
                  </p>
                  {s.declared_allowed_tools && s.declared_allowed_tools.length > 0 ? (
                    <p className="mt-1.5 text-[0.6875rem] text-[var(--color-text-muted)]">
                      <span className="uppercase tracking-wide">Declared tools</span>{" "}
                      {s.declared_allowed_tools.join(", ")}{" "}
                      <span title="Declared for review only — fleet does not enforce a skill's allowed-tools. The real limits are the sandbox, MCP allowlist, and approval gate.">
                        (advisory)
                      </span>
                    </p>
                  ) : null}
                  {openSkill === s.name ? (
                    detail ? (
                      <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap rounded-[0.75rem] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] p-3 text-[0.75rem] leading-relaxed text-[var(--color-text-secondary)]">
                        {detail.content}
                      </pre>
                    ) : (
                      <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
                        Loading…
                      </p>
                    )
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </div>

        <p className="mt-4 text-[0.75rem] text-[var(--color-text-muted)]">
          Skills are files in your workspace&apos;s config bundle
          (skills/&lt;name&gt;/SKILL.md); the built-in pack ships with fleet and
          can be trimmed with the bundle&apos;s{" "}
          <code className="rounded bg-[var(--color-overlay-soft)] px-1 py-0.5">
            skills_hidden
          </code>{" "}
          /{" "}
          <code className="rounded bg-[var(--color-overlay-soft)] px-1 py-0.5">
            skills_builtin
          </code>{" "}
          knobs.
        </p>
      </>
    </SettingsShell>
  );
}
