"use client";

import Image from "next/image";
import Link from "next/link";
import { useEffect, useState } from "react";

import { StatusChip } from "@/app/shared/ui/StatusChip";

// Skills library (Settings → Skills). Browses every Agent Skill loaded on
// this deployment — the operator's bundle skills plus fleet's built-in pack —
// with search and a full SKILL.md read view. Skills are invoked from chat by
// starting a message with "/<skill-name>" (the composer autocompletes them),
// or picked up automatically when a task matches a skill's description.

type SkillEntry = {
  name: string;
  description: string;
  source: string; // "bundle" | "builtin"
};

type SkillDetail = SkillEntry & { content: string };

export default function SkillsPage() {
  const [skills, setSkills] = useState<SkillEntry[] | null>(null);
  const [query, setQuery] = useState("");
  const [openSkill, setOpenSkill] = useState<string | null>(null);
  const [detail, setDetail] = useState<SkillDetail | null>(null);
  const [error, setError] = useState<string | null>(null);

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
    return () => {
      stale = true;
    };
  }, []);

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
    <main className="h-dvh overflow-y-auto bg-[var(--gradient-bg-home-signature)] px-6 py-10 text-[var(--color-text-primary)]">
      <div className="mx-auto w-full max-w-3xl">
        <header className="mb-6 flex items-center justify-between gap-4">
          <Link href="/" className="flex items-center gap-2.5 no-underline">
            <Image
              src="/logos/elcano-mark-primary.svg"
              alt="Elcano"
              width={28}
              height={28}
              priority
            />
            <span className="font-heading text-[0.9375rem] font-semibold">
              Skills
            </span>
          </Link>
          <Link
            href="/"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Back to chat
          </Link>
        </header>

        <p className="mb-5 text-[0.875rem] text-[var(--color-text-secondary)]">
          Packaged, on-demand capabilities the agent can pick up when a task
          matches — your workspace&apos;s own skills plus fleet&apos;s built-in
          pack. Invoke one explicitly by starting a chat message with{" "}
          <code className="rounded bg-[var(--color-overlay-soft)] px-1 py-0.5 text-[0.8125rem]">
            /skill-name
          </code>
          .
        </p>

        {error ? (
          <p className="mb-4 text-[0.8125rem] text-[var(--color-danger-soft)]">
            {error}
          </p>
        ) : null}

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
      </div>
    </main>
  );
}
