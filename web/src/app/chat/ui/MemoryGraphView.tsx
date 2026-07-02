"use client";

import { useCallback, useEffect, useState } from "react";
import {
  buildGraphQueryString,
  groupRelationsBySubject,
  relationObjectLabel,
  relationValiditySuffix,
  type GraphResponse,
} from "./memoryGraph";

// MemoryGraphView (#523): the compact "Graph" tab of the memory-manager modal.
// Renders the derived knowledge graph as relation rows grouped by subject
// entity ("Ada — works at → Elcano Corp"), with the source memory's snippet as
// a subline, plus two optional datetime-local inputs that time-travel the two
// bi-temporal axes (valid time = "what was true on…", transaction time =
// "what did fleet know on…"). Self-contained: owns its fetching against the
// /api/memories/graph proxy.

export function MemoryGraphView() {
  const [graph, setGraph] = useState<GraphResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [asOfValid, setAsOfValid] = useState("");
  const [asOfLearned, setAsOfLearned] = useState("");

  const loadGraph = useCallback(async (valid: string, learned: string) => {
    setLoading(true);
    setError(null);
    try {
      const response = await fetch(`/api/memories/graph${buildGraphQueryString(valid, learned)}`, {
        cache: "no-store",
      });
      if (!response.ok) throw new Error(`Graph request failed (${response.status}).`);
      setGraph((await response.json()) as GraphResponse);
    } catch (err) {
      setError(err instanceof Error && err.message ? err.message : "Unable to load the graph.");
    } finally {
      setLoading(false);
    }
  }, []);

  // Deferred like ProjectsModal's loaders: queueMicrotask keeps the effect
  // body free of synchronous setState (react-hooks/set-state-in-effect).
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void loadGraph(asOfValid, asOfLearned);
    });
    return () => {
      cancelled = true;
    };
  }, [loadGraph, asOfValid, asOfLearned]);

  const groups = groupRelationsBySubject(graph?.relations ?? []);
  const timeTravelling = asOfValid !== "" || asOfLearned !== "";

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="grid gap-2 sm:grid-cols-2">
        <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-muted)]">
          What was true on… (valid time)
          <input
            type="datetime-local"
            className="rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            value={asOfValid}
            onChange={(event) => setAsOfValid(event.target.value)}
          />
        </label>
        <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-muted)]">
          What did fleet know on… (learned time)
          <input
            type="datetime-local"
            className="rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            value={asOfLearned}
            onChange={(event) => setAsOfLearned(event.target.value)}
          />
        </label>
      </div>

      {error ? (
        <div className="rounded-[0.75rem] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger,#dc2626)]">
          {error}
        </div>
      ) : null}

      <div className="min-h-0 flex-1 overflow-y-auto pr-1">
        {loading ? (
          <p className="py-4 text-[0.8125rem] text-[var(--color-text-muted)]">Loading graph...</p>
        ) : groups.length === 0 ? (
          <p className="rounded-[0.9rem] border border-dashed border-[var(--color-border)] px-3 py-4 text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
            {timeTravelling
              ? "Nothing was known/true at that point in time."
              : "No graph yet. fleet can derive an entity/relation graph from saved memories — ask your operator to set FLEET_MEMORY_GRAPH_ENABLED=true, then newly saved memories are mined automatically."}
          </p>
        ) : (
          <div className="grid gap-2">
            {groups.map((group) => (
              <div
                key={group.subject.id}
                className="rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3"
              >
                <p className="text-[0.875rem] font-medium text-[var(--color-text-primary)]">
                  {group.subject.name}
                  <span className="ml-1.5 rounded-full border border-[var(--color-border)] px-1.5 py-0.5 text-[0.65rem] font-normal text-[var(--color-text-muted)]">
                    {group.subject.type}
                  </span>
                </p>
                <ul className="mt-1.5 grid gap-1">
                  {group.relations.map((rel) => {
                    const validity = relationValiditySuffix(rel);
                    return (
                      <li
                        key={rel.id}
                        className="text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]"
                        title={rel.memory_content_snippet}
                      >
                        <span className="text-[var(--color-text-muted)]">{rel.predicate}</span>{" "}
                        → <span className="text-[var(--color-text-primary)]">{relationObjectLabel(rel)}</span>
                        {validity ? (
                          <span className="ml-1.5 text-[0.7rem] text-[var(--color-text-muted)]">({validity})</span>
                        ) : null}
                        <span className="block truncate text-[0.7rem] text-[var(--color-text-muted)]">
                          from: {rel.memory_content_snippet}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
