"use client";

import { useEffect, useState } from "react";

import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { StatusChip } from "@/app/shared/ui/StatusChip";

// Model providers panel — the admin surface for admin-managed LLM providers.
// Add an OpenRouter/Anthropic/OpenAI API key or point at any local
// OpenAI-compatible endpoint (Ollama, vLLM, LM Studio…); rows overlay the
// client bundle's providers: table and take effect on the next turn, no
// restart. Multiple providers run simultaneously — routing picks per model
// slug (an explicit "name/model" prefix, a provider's models list, else the
// first catch-all).
//
// SECURITY: API-key values are WRITE-ONLY, mirroring MCP credential accounts.
// The list reports only whether a key is stored; the edit form's key field
// always starts empty and blank means "keep the stored key".

export type LLMProvider = {
  id: string;
  name: string;
  type: string;
  base_url: string;
  models: string[];
  enabled: boolean;
  has_api_key: boolean;
  created_at: number;
  updated_at: number;
};

// Central per-type presentation map (label + one-line hint + placeholder),
// so the form can explain each choice inline.
const PROVIDER_TYPES: Record<
  string,
  { label: string; hint: string; urlPlaceholder: string; keyNeeded: boolean }
> = {
  openrouter: {
    label: "OpenRouter",
    hint: "Routes across many upstream providers with one key.",
    urlPlaceholder: "(default: openrouter.ai)",
    keyNeeded: true,
  },
  anthropic: {
    label: "Anthropic",
    hint: "Direct Anthropic API access with your own key.",
    urlPlaceholder: "(default: api.anthropic.com)",
    keyNeeded: true,
  },
  openai: {
    label: "OpenAI / OpenAI-compatible",
    hint: "OpenAI itself, or any OpenAI-compatible endpoint via Base URL (vLLM, LM Studio, a gateway…).",
    urlPlaceholder: "https://api.example.com/v1",
    keyNeeded: true,
  },
  ollama: {
    label: "Ollama (local)",
    hint: "A local or remote Ollama server; no API key needed.",
    urlPlaceholder: "(default: http://localhost:11434/v1)",
    keyNeeded: false,
  },
};

type Draft = {
  id: string; // "" = creating
  name: string;
  type: string;
  base_url: string;
  api_key: string; // always starts empty; "" on edit = keep stored key
  models: string; // textarea, one slug per line
  enabled: boolean;
};

const EMPTY_DRAFT: Draft = {
  id: "",
  name: "",
  type: "openrouter",
  base_url: "",
  api_key: "",
  models: "",
  enabled: true,
};

function draftFrom(p: LLMProvider): Draft {
  return {
    id: p.id,
    name: p.name,
    type: p.type,
    base_url: p.base_url,
    api_key: "",
    models: p.models.join("\n"),
    enabled: p.enabled,
  };
}

// validateDraft mirrors the server's checks so most mistakes surface before a
// round trip: slug name, http(s) base URL, key presence for keyed types.
function validateDraft(d: Draft, editingHasKey: boolean): string | null {
  if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(d.name.trim().toLowerCase())) {
    return "Name must be a lowercase slug (letters, digits, . _ -), no slashes or spaces.";
  }
  if (d.base_url.trim() && !/^https?:\/\//.test(d.base_url.trim())) {
    return "Base URL must start with http:// or https://.";
  }
  const t = PROVIDER_TYPES[d.type];
  if (d.enabled && t?.keyNeeded && !d.api_key && !(d.id && editingHasKey)) {
    return `${t.label} needs an API key.`;
  }
  return null;
}

async function fetchProviders(): Promise<LLMProvider[] | null> {
  const response = await fetch("/api/admin/llm-providers", { cache: "no-store" });
  if (response.status === 401) {
    window.location.href = "/login";
    return null;
  }
  if (response.status === 403) {
    throw new Error("You are not an admin.");
  }
  if (!response.ok) {
    throw new Error(`Providers request failed: ${response.status}`);
  }
  const data = (await response.json()) as { providers?: LLMProvider[] };
  return data.providers ?? [];
}

// A finished test-connection probe for one row, as the backend reports it.
type ProbeResult = {
  ok: boolean;
  status?: number;
  detail: string;
  served_model_count?: number;
  missing_models?: string[];
  latency_ms: number;
};

export function ProvidersPanel() {
  const [providers, setProviders] = useState<LLMProvider[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [busy, setBusy] = useState(false);
  // Per-row probe state: "running" while in flight, else the last result.
  const [probes, setProbes] = useState<Record<string, "running" | ProbeResult>>({});

  useEffect(() => {
    let stale = false;
    fetchProviders()
      .then((rows) => {
        if (!stale && rows !== null) setProviders(rows);
      })
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : "Failed to load.");
      });
    return () => {
      stale = true;
    };
  }, []);

  const reload = () =>
    fetchProviders()
      .then((rows) => {
        if (rows !== null) setProviders(rows);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Failed to load."));

  const save = () => {
    if (!draft) return;
    const editing = providers?.find((p) => p.id === draft.id);
    const invalid = validateDraft(draft, editing?.has_api_key ?? false);
    if (invalid) {
      setError(invalid);
      return;
    }
    // Empty models = catch-all is powerful enough to hijack every unmatched
    // slug — make the admin say so out loud instead of falling into it.
    if (
      draft.enabled &&
      draft.models.split("\n").every((s) => !s.trim()) &&
      !window.confirm(
        "No models listed — this provider becomes a CATCH-ALL and may serve any model not claimed by another provider. Continue?",
      )
    ) {
      return;
    }
    setError(null);
    setBusy(true);
    const body: Record<string, unknown> = {
      name: draft.name.trim().toLowerCase(),
      type: draft.type,
      base_url: draft.base_url.trim(),
      models: draft.models
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean),
      enabled: draft.enabled,
    };
    // Write-only key semantics: omit the field entirely to keep the stored
    // key; send a value only when the admin typed one.
    if (draft.api_key !== "") body.api_key = draft.api_key;
    else if (!draft.id) body.api_key = "";
    fetch(draft.id ? `/api/admin/llm-providers/${encodeURIComponent(draft.id)}` : "/api/admin/llm-providers", {
      method: draft.id ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Save failed: ${res.status}`);
        setDraft(null);
        return reload();
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Save failed."))
      .finally(() => setBusy(false));
  };

  const toggle = (p: LLMProvider) => {
    setError(null);
    setBusy(true);
    fetch(`/api/admin/llm-providers/${encodeURIComponent(p.id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: p.name,
        type: p.type,
        base_url: p.base_url,
        models: p.models,
        enabled: !p.enabled,
      }),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Update failed: ${res.status}`);
        return reload();
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Update failed."))
      .finally(() => setBusy(false));
  };

  // Test connection: one host-side probe against the provider's real endpoint
  // (key check + model-catalog cross-check where the type supports it). Works
  // on disabled rows, so an admin can verify before enabling.
  const test = (p: LLMProvider) => {
    setProbes((cur) => ({ ...cur, [p.id]: "running" }));
    fetch(`/api/admin/llm-providers/${encodeURIComponent(p.id)}/test`, { method: "POST" })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Test failed: ${res.status}`);
        return (await res.json()) as ProbeResult;
      })
      .then((result) => setProbes((cur) => ({ ...cur, [p.id]: result })))
      .catch((err: unknown) =>
        setProbes((cur) => ({
          ...cur,
          [p.id]: {
            ok: false,
            detail: err instanceof Error ? err.message : "Test failed.",
            latency_ms: 0,
          },
        })),
      );
  };

  const remove = (p: LLMProvider) => {
    if (!window.confirm(`Remove provider "${p.name}"? Its stored key is deleted with it.`)) return;
    setError(null);
    setBusy(true);
    fetch(`/api/admin/llm-providers/${encodeURIComponent(p.id)}`, { method: "DELETE" })
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Delete failed: ${res.status}`);
        return reload();
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Delete failed."))
      .finally(() => setBusy(false));
  };

  const typeInfo = draft ? PROVIDER_TYPES[draft.type] : null;

  return (
    <div className="mt-6 overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
      <div className="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2">
        <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
          Model providers
        </span>
        <button
          type="button"
          onClick={() => setDraft({ ...EMPTY_DRAFT })}
          disabled={busy}
          className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
        >
          Add provider
        </button>
      </div>
      <p className="px-4 pt-3 text-[0.75rem] text-[var(--color-text-muted)]">
        LLM backends this deployment can route models through — an OpenRouter,
        Anthropic, or OpenAI key, or any OpenAI-compatible endpoint (Ollama,
        vLLM…). Several can run at once: a model picks its provider by explicit{" "}
        <code className="rounded bg-[var(--color-overlay-soft)] px-1">provider/model</code>{" "}
        prefix or by its models list; a provider with an empty models list is a
        catch-all serving any slug. Changes apply on the next message — no
        restart. Keys are stored encrypted and are never shown again.
      </p>

      {error ? (
        <div className="px-4 pt-3">
          <NoticeBanner tone="danger">{error}</NoticeBanner>
        </div>
      ) : null}

      {draft ? (
        <div className="border-b border-[var(--color-border-subtle)] px-4 py-3">
          <div className="grid gap-2">
            <div className="grid gap-2 sm:grid-cols-2">
              <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                Name (routing prefix{draft.id ? " — fixed after creation" : ", e.g. anthropic-direct"})
                <input
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                  placeholder="my-provider"
                  // Immutable once created: the name is baked into saved
                  // conversations'/tasks' "name/model" slugs (server enforces too).
                  disabled={draft.id !== ""}
                  className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)] disabled:opacity-60"
                />
              </label>
              <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                Type
                <select
                  value={draft.type}
                  onChange={(e) => setDraft({ ...draft, type: e.target.value })}
                  className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                >
                  {Object.entries(PROVIDER_TYPES).map(([value, t]) => (
                    <option key={value} value={value}>
                      {t.label}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            {typeInfo ? (
              <p className="text-[0.75rem] text-[var(--color-text-muted)]">{typeInfo.hint}</p>
            ) : null}
            <div className="grid gap-2 sm:grid-cols-2">
              <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                Base URL {draft.type === "openai" ? "(set for a non-OpenAI endpoint)" : "(optional)"}
                <input
                  value={draft.base_url}
                  onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
                  placeholder={typeInfo?.urlPlaceholder}
                  className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                />
              </label>
              <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
                API key{" "}
                {draft.id && providers?.find((p) => p.id === draft.id)?.has_api_key
                  ? "(stored — leave blank to keep it)"
                  : typeInfo?.keyNeeded
                    ? ""
                    : "(not needed)"}
                <input
                  type="password"
                  autoComplete="new-password"
                  value={draft.api_key}
                  onChange={(e) => setDraft({ ...draft, api_key: e.target.value })}
                  placeholder={
                    draft.id && providers?.find((p) => p.id === draft.id)?.has_api_key
                      ? "•••••••• (unchanged)"
                      : "sk-…"
                  }
                  className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                />
              </label>
            </div>
            <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
              Models (one slug per line — these appear in everyone&apos;s model picker as{" "}
              <code className="rounded bg-[var(--color-overlay-soft)] px-1">
                {draft.name.trim() || "name"}/model
              </code>
              ; leave empty to serve any model as a catch-all)
              <textarea
                value={draft.models}
                onChange={(e) => setDraft({ ...draft, models: e.target.value })}
                rows={3}
                placeholder={"claude-sonnet-4-5\nclaude-opus-4-8"}
                className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 font-mono text-[0.8125rem] leading-relaxed text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
            </label>
            <div className="flex items-center justify-between gap-2">
              <label className="flex items-center gap-2 text-[0.8125rem] text-[var(--color-text-secondary)]">
                <input
                  type="checkbox"
                  checked={draft.enabled}
                  onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
                />
                Enabled
              </label>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => {
                    setDraft(null);
                    setError(null);
                  }}
                  disabled={busy}
                  className="rounded-full border border-[var(--color-border-subtle)] px-4 py-1.5 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  Cancel
                </button>
                <button
                  type="button"
                  onClick={save}
                  disabled={busy || !draft.name.trim()}
                  className="rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  {busy ? "Saving…" : draft.id === "" ? "Create provider" : "Save changes"}
                </button>
              </div>
            </div>
          </div>
        </div>
      ) : null}

      {providers === null ? (
        <p className="px-4 py-4 text-center text-[0.875rem] text-[var(--color-text-muted)]">
          Loading…
        </p>
      ) : providers.length === 0 && !draft ? (
        <p className="px-4 py-4 text-center text-[0.875rem] text-[var(--color-text-muted)]">
          No extra providers configured — the deployment is using its
          bundle/environment provider (typically OpenRouter). Add one to bring
          your own Anthropic/OpenAI key or a local endpoint.
        </p>
      ) : (
        <ul>
          {providers.map((p) => (
            <li
              key={p.id}
              className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
            >
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{p.name}</span>
                  <StatusChip tone="neutral">{PROVIDER_TYPES[p.type]?.label ?? p.type}</StatusChip>
                  {p.enabled ? (
                    <StatusChip tone="success">Enabled</StatusChip>
                  ) : (
                    <StatusChip tone="neutral">Disabled</StatusChip>
                  )}
                  {p.has_api_key ? (
                    <StatusChip tone="success">Key stored</StatusChip>
                  ) : PROVIDER_TYPES[p.type]?.keyNeeded ? (
                    <StatusChip tone="warning">No key</StatusChip>
                  ) : null}
                </div>
                <p className="mt-0.5 truncate text-[0.75rem] text-[var(--color-text-muted)]">
                  {p.base_url || "default endpoint"}
                  {" · "}
                  {p.models.length > 0 ? `${p.models.length} model${p.models.length === 1 ? "" : "s"}` : "catch-all (any model)"}
                </p>
                {probes[p.id] && probes[p.id] !== "running" ? (
                  <p
                    className={`mt-0.5 text-[0.75rem] ${
                      (probes[p.id] as ProbeResult).ok
                        ? "text-[var(--color-success-soft)]"
                        : "text-[var(--color-danger-soft)]"
                    }`}
                    data-testid={`probe-result-${p.id}`}
                  >
                    {(probes[p.id] as ProbeResult).ok ? "✓" : "✗"}{" "}
                    {(probes[p.id] as ProbeResult).detail}
                    {(probes[p.id] as ProbeResult).latency_ms > 0
                      ? ` · ${(probes[p.id] as ProbeResult).latency_ms}ms`
                      : ""}
                    {((probes[p.id] as ProbeResult).missing_models?.length ?? 0) > 0
                      ? ` — missing: ${(probes[p.id] as ProbeResult).missing_models!.join(", ")}`
                      : ""}
                  </p>
                ) : null}
              </div>
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => test(p)}
                  disabled={busy || probes[p.id] === "running"}
                  title="One authenticated call against the provider's endpoint — verifies the key/base URL and cross-checks the listed models."
                  className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  {probes[p.id] === "running" ? "Testing…" : "Test"}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setError(null);
                    setDraft(draftFrom(p));
                  }}
                  disabled={busy}
                  className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  Edit
                </button>
                <button
                  type="button"
                  onClick={() => toggle(p)}
                  disabled={busy}
                  className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  {p.enabled ? "Disable" : "Enable"}
                </button>
                <button
                  type="button"
                  onClick={() => remove(p)}
                  disabled={busy}
                  className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                >
                  Remove
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export default ProvidersPanel;
