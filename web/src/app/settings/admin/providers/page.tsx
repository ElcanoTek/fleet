"use client";

// Settings → Admin → Providers (fleet-unified settings pass): the admin
// surface for admin-managed LLM providers. Add an OpenRouter/Anthropic/OpenAI/
// Vercel AI Gateway API key or point at any OpenAI-compatible endpoint (Ollama,
// vLLM, LM Studio…); rows overlay the client bundle's providers: table and take
// effect on the next turn, no restart. Multiple providers run simultaneously —
// routing picks per model slug (an explicit "name/model" prefix, a provider's
// models list, else the first catch-all).
//
// SECURITY: API-key values are WRITE-ONLY, mirroring MCP credential accounts.
// The list reports only whether a key is stored; the edit form's key field
// always starts empty and blank means "keep the stored key".

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useRouter } from "@/app/lib/navigation";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import {
  ActStatus,
  btnClass,
  CodeChip,
  ConnBadge,
  InlineConfirmButton,
  RevealButton,
  SETTINGS_INPUT,
} from "../../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnPanel,
  ConnPanelHead,
  ConnRow,
  ConnRows,
  SetSection,
} from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { AdminGateFallback } from "../../AdminGateFallback";

export type LLMProvider = {
  id: string;
  name: string;
  type: string;
  base_url: string;
  models: string[];
  enabled: boolean;
  has_api_key: boolean;
  // The endpoint's declared context limit; 0 = not declared.
  context_window_tokens: number;
  created_at: number;
  updated_at: number;
};

// Central per-type presentation map (label + one-line hint + field labels),
// so the form can explain each choice inline. The keys must match the server's
// provider types (agentcore's provider type table); a scripts/ test pins them,
// and keyNeeded/urlNeeded mirror that table's required fields. windowNeeded is
// false only for OpenRouter, whose per-model catalog supplies the context
// limit; every other type needs the admin to declare it before the row can be
// enabled (the server enforces this too).
const PROVIDER_TYPES: Record<
  string,
  {
    label: string;
    hint: string;
    urlLabel: string;
    urlPlaceholder: string;
    urlNeeded: boolean;
    keyLabel: string;
    keyNeeded: boolean;
    windowNeeded: boolean;
    windowPlaceholder: string;
  }
> = {
  openrouter: {
    label: "OpenRouter",
    hint: "Routes across many upstream providers with one key.",
    urlLabel: "Base URL (optional)",
    urlPlaceholder: "(default: openrouter.ai)",
    urlNeeded: false,
    keyLabel: "API key",
    keyNeeded: true,
    windowNeeded: false,
    windowPlaceholder: "",
  },
  anthropic: {
    label: "Anthropic",
    hint: "Direct Anthropic API access with your own key.",
    urlLabel: "Base URL (optional)",
    urlPlaceholder: "(default: api.anthropic.com)",
    urlNeeded: false,
    keyLabel: "API key",
    keyNeeded: true,
    windowNeeded: true,
    windowPlaceholder: "e.g. 200000",
  },
  openai: {
    label: "OpenAI",
    hint: "Direct OpenAI API access with your own key. For another server that speaks the OpenAI API, choose OpenAI-compatible.",
    urlLabel: "Base URL (optional)",
    urlPlaceholder: "(default: api.openai.com/v1)",
    urlNeeded: false,
    keyLabel: "API key",
    keyNeeded: true,
    windowNeeded: true,
    windowPlaceholder: "e.g. 128000",
  },
  ollama: {
    label: "Ollama (local)",
    hint: "A local or remote Ollama server; no API key needed.",
    urlLabel: "Base URL (optional)",
    urlPlaceholder: "(default: http://localhost:11434/v1)",
    urlNeeded: false,
    keyLabel: "API key (not needed)",
    keyNeeded: false,
    windowNeeded: true,
    windowPlaceholder: "the server's num_ctx, e.g. 65536",
  },
  openaicompat: {
    label: "OpenAI-compatible",
    hint: "Any server that speaks the OpenAI API: vLLM, LM Studio, llama.cpp, a hosted gateway. Needs its Base URL; the key is optional.",
    urlLabel: "Base URL",
    urlPlaceholder: "https://llm.example.com/v1",
    urlNeeded: true,
    keyLabel: "API key (optional)",
    keyNeeded: false,
    windowNeeded: true,
    windowPlaceholder: "the server's limit, e.g. 131072",
  },
  vercel: {
    label: "Vercel AI Gateway",
    hint: "Many model providers through one Vercel key. Model ids are creator/model, like anthropic/claude-opus-4.8.",
    urlLabel: "Base URL (optional)",
    urlPlaceholder: "(default: ai-gateway.vercel.sh/v1)",
    urlNeeded: false,
    keyLabel: "API key",
    keyNeeded: true,
    windowNeeded: true,
    windowPlaceholder: "e.g. 200000",
  },
};

type Draft = {
  id: string; // "" = creating
  name: string;
  type: string;
  base_url: string;
  api_key: string; // always starts empty; "" on edit = keep stored key
  models: string; // textarea, one slug per line
  context_window: string; // digits; "" = not declared
  enabled: boolean;
};

const EMPTY_DRAFT: Draft = {
  id: "",
  name: "",
  type: "openrouter",
  base_url: "",
  api_key: "",
  models: "",
  context_window: "",
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
    context_window: p.context_window_tokens > 0 ? String(p.context_window_tokens) : "",
    enabled: p.enabled,
  };
}

// The server stores the window in a Postgres INTEGER.
const MAX_CONTEXT_WINDOW = 2147483647;

// A turn reserves fleet's tool schemas, system prompt and reply allowance
// before any history: a little under 35,000 tokens with the default tool set
// as of migration 082. Below that a turn usually cannot run at all; below the
// second tier it runs with little room for the conversation. The form warns
// without refusing, because the real reserve depends on the persona and its
// tools.
const SMALL_CONTEXT_WINDOW = 35000;
const TIGHT_CONTEXT_WINDOW = 64000;

function contextWindowWarning(raw: string): string | null {
  const n = Number(raw.trim());
  if (!(n > 0)) return null;
  if (n < SMALL_CONTEXT_WINDOW) {
    return `Below about ${SMALL_CONTEXT_WINDOW.toLocaleString()} tokens a turn usually cannot fit fleet's tools and reply; for Ollama, raise num_ctx on the server.`;
  }
  if (n < TIGHT_CONTEXT_WINDOW) {
    return `Below about ${TIGHT_CONTEXT_WINDOW.toLocaleString()} tokens little room is left for the conversation itself once fleet's tools and reply are reserved.`;
  }
  return null;
}

// validateDraft mirrors the server's checks so most mistakes surface before a
// round trip: slug name, http(s) base URL, base URL presence for types with no
// default endpoint, key presence for keyed types, and a context window before a
// non-OpenRouter row can be enabled.
function validateDraft(d: Draft, editingHasKey: boolean): string | null {
  if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(d.name.trim().toLowerCase())) {
    return "Name must be a lowercase slug (letters, digits, . _ -), no slashes or spaces.";
  }
  if (d.base_url.trim() && !/^https?:\/\//.test(d.base_url.trim())) {
    return "Base URL must start with http:// or https://.";
  }
  const t = PROVIDER_TYPES[d.type];
  if (t?.urlNeeded && !d.base_url.trim()) {
    return `${t.label} needs a Base URL.`;
  }
  if (d.enabled && t?.keyNeeded && !d.api_key && !(d.id && editingHasKey)) {
    return `${t.label} needs an API key.`;
  }
  if (t?.windowNeeded) {
    const w = d.context_window.trim();
    if (w && (!/^[0-9]+$/.test(w) || Number(w) <= 0)) {
      return "Context window must be a positive whole number of tokens.";
    }
    if (Number(w) > MAX_CONTEXT_WINDOW) {
      return `Context window must be at most ${MAX_CONTEXT_WINDOW.toLocaleString()} tokens.`;
    }
    if (d.enabled && !w) {
      return `${t.label} needs its context window (in tokens: the smallest limit among the models it serves) before it can be enabled.`;
    }
  }
  return null;
}

// contextWindowFor is the token count the form sends: the declared number, or
// 0 (not declared) for OpenRouter, which takes the limit from its catalog.
function contextWindowFor(d: Draft): number {
  if (!PROVIDER_TYPES[d.type]?.windowNeeded) return 0;
  return Number.parseInt(d.context_window.trim(), 10) || 0;
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

// ClampInfo — the explainer clamped to ~3 lines with an expand affordance.
// Local (not the shared ClampText) because the copy carries inline <code>
// markup, which ClampText's plain-text prop can't render; the measured-clamp
// pattern is the same: "more" appears only when the text actually overflows.
function ClampInfo({ children }: { children: ReactNode }) {
  const [expanded, setExpanded] = useState(false);
  const [clamped, setClamped] = useState(false);
  const ref = useRef<HTMLParagraphElement | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setClamped(el.scrollHeight > el.clientHeight + 1);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [expanded]);

  return (
    <div className="mb-[0.9rem] mt-[0.4rem] min-w-0">
      <p
        ref={ref}
        className={[
          "m-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]",
          expanded ? "" : "line-clamp-3",
        ].join(" ")}
      >
        {children}
      </p>
      {clamped || expanded ? (
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() => setExpanded((e) => !e)}
          className="mt-[0.15rem] border-none bg-transparent p-0 text-[0.72rem] text-[var(--color-accent)] underline-offset-2 hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {expanded ? "less" : "more"}
        </button>
      ) : null}
    </div>
  );
}

export default function ProvidersAdminPage() {
  // Client-side visibility gate only — every /api/admin call below is
  // independently authorized server-side regardless of what renders here.
  const admin = useIsAdmin();
  const router = useRouter();
  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);
  if (admin !== "admin") return <AdminGateFallback state={admin} />;
  return <ProvidersAdmin />;
}

function ProvidersAdmin() {
  const [providers, setProviders] = useState<LLMProvider[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [busy, setBusy] = useState(false);
  // Catch-all confirmation: an enabled provider with no models listed can
  // hijack every unmatched slug, so Save arms on the first click and fires on
  // the second — the same inline two-click idiom as InlineConfirmButton,
  // instead of a native window.confirm dialog. Blur/Escape/Cancel disarm.
  const [catchAllArmed, setCatchAllArmed] = useState(false);
  // Set when Enable opened the form for a row with no context window: the
  // field it needs gets focus (and is scrolled to) once it renders.
  const focusWindowRef = useRef(false);
  useEffect(() => {
    if (!focusWindowRef.current || !draft) return;
    focusWindowRef.current = false;
    const input = document.getElementById("providerContextWindow");
    input?.focus();
    input?.scrollIntoView?.({ block: "center" });
  }, [draft]);
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

  // Retry after a failed initial load: clear the banner so a second failure
  // is visibly a fresh one, then re-run the same fetch.
  const retryLoad = () => {
    setError(null);
    void reload();
  };

  const save = () => {
    if (!draft) return;
    const editing = providers?.find((p) => p.id === draft.id);
    const invalid = validateDraft(draft, editing?.has_api_key ?? false);
    if (invalid) {
      setError(invalid);
      return;
    }
    // Empty models = catch-all is powerful enough to hijack every unmatched
    // slug — make the admin say so out loud instead of falling into it: the
    // first Save click only arms the button (with the warning inline), the
    // second one actually submits.
    const catchAll = draft.enabled && draft.models.split("\n").every((s) => !s.trim());
    if (catchAll && !catchAllArmed) {
      setCatchAllArmed(true);
      return;
    }
    setCatchAllArmed(false);
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
      context_window_tokens: contextWindowFor(draft),
      enabled: draft.enabled,
    };
    // Write-only key semantics: omit the field entirely to keep the stored
    // key; send a value only when the admin typed one.
    if (draft.api_key !== "") body.api_key = draft.api_key;
    else if (!draft.id) body.api_key = "";
    fetch(
      draft.id
        ? `/api/admin/llm-providers/${encodeURIComponent(draft.id)}`
        : "/api/admin/llm-providers",
      {
        method: draft.id ? "PUT" : "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      },
    )
      .then(async (res) => {
        if (!res.ok) throw new Error((await res.text()) || `Save failed: ${res.status}`);
        setDraft(null);
        return reload();
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Save failed."))
      .finally(() => setBusy(false));
  };

  const toggle = (p: LLMProvider) => {
    // Enabling a row saved without a context window would be refused by the
    // server; open its edit form on the missing field instead.
    if (!p.enabled && PROVIDER_TYPES[p.type]?.windowNeeded && p.context_window_tokens <= 0) {
      setDraft({ ...draftFrom(p), enabled: true });
      focusWindowRef.current = true;
      setError(
        `Set ${p.name}'s context window (in tokens: the smallest limit among the models it serves), then save to enable it.`,
      );
      return;
    }
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
        context_window_tokens: p.context_window_tokens,
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

  // Remove is confirmed inline (InlineConfirmButton arms on first click) —
  // no window.confirm; the stored key is deleted with the row.
  const remove = (p: LLMProvider) => {
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
    <SetSection
      title="Providers"
      intro="LLM backends this deployment can route models through — several can run at once."
    >
      <ConnPanel>
        <ConnPanelHead title="Model providers">
          <RevealButton
            open={draft !== null}
            closedLabel="Add provider"
            onClick={() => {
              if (draft) {
                setDraft(null);
                setError(null);
              } else {
                setDraft({ ...EMPTY_DRAFT });
              }
            }}
            disabled={busy}
          />
        </ConnPanelHead>
        <ClampInfo>
          An OpenRouter, Anthropic, OpenAI or Vercel AI Gateway key, or any OpenAI-compatible
          endpoint (Ollama, vLLM…). Several can run at once: a model picks its provider by explicit{" "}
          <CodeChip>provider/model</CodeChip> prefix or by its models list; a provider with an
          empty models list is a catch-all serving any slug. Changes apply on the next message —
          no restart. Keys are stored encrypted and are never shown again.
        </ClampInfo>

        {error ? (
          <div className="mb-3 flex items-center gap-3">
            <NoticeBanner tone="danger" className="flex-1">
              {error}
            </NoticeBanner>
            {providers === null ? (
              // The list never loaded: give the failure a real Retry instead
              // of a dead "Loading…" line under the banner.
              <button
                type="button"
                onClick={retryLoad}
                data-testid="providers-retry"
                className={btnClass({ sm: true, reveal: true })}
              >
                Retry
              </button>
            ) : null}
          </div>
        ) : null}

        {draft ? (
          <div className="mb-4 grid gap-[0.75rem] border-b border-[var(--color-border-subtle)] pb-4">
            <div className="grid grid-cols-2 gap-[0.75rem] max-[640px]:grid-cols-1">
              <ConnField
                label={
                  draft.id
                    ? "Name (routing prefix — fixed after creation)"
                    : "Name (routing prefix, e.g. anthropic-direct)"
                }
              >
                <input
                  id="providerName"
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                  placeholder="my-provider"
                  // Immutable once created: the name is baked into saved
                  // conversations'/tasks' "name/model" slugs (server enforces too).
                  disabled={draft.id !== ""}
                  className={`${SETTINGS_INPUT} disabled:opacity-60`}
                />
              </ConnField>
              <ConnField label="Type">
                <span className="select-wrap block">
                  <select
                    id="providerType"
                    value={draft.type}
                    // A new row's window belongs to the type it was typed for
                    // (Ollama's is the server's num_ctx), so a type change
                    // clears it; an edit keeps the stored value.
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        type: e.target.value,
                        context_window: draft.id ? draft.context_window : "",
                      })
                    }
                    // pr overrides the base px and needs `!` under Tailwind
                    // v4's fixed utility ordering.
                    className={`${SETTINGS_INPUT} appearance-none pr-8!`}
                  >
                    {Object.entries(PROVIDER_TYPES).map(([value, t]) => (
                      <option key={value} value={value}>
                        {t.label}
                      </option>
                    ))}
                  </select>
                </span>
              </ConnField>
            </div>
            {typeInfo ? (
              <p className="-mt-1 mb-0 text-[0.73rem] text-[var(--color-text-muted)]">
                {typeInfo.hint}
              </p>
            ) : null}
            <div className="grid grid-cols-2 gap-[0.75rem] max-[640px]:grid-cols-1">
              <ConnField label={typeInfo?.urlLabel ?? "Base URL (optional)"}>
                <input
                  id="providerBaseUrl"
                  value={draft.base_url}
                  onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
                  placeholder={typeInfo?.urlPlaceholder}
                  className={SETTINGS_INPUT}
                />
              </ConnField>
              <ConnField
                label={
                  draft.id && providers?.find((p) => p.id === draft.id)?.has_api_key
                    ? "API key (stored — leave blank to keep it)"
                    : (typeInfo?.keyLabel ?? "API key")
                }
              >
                <input
                  id="providerApiKey"
                  type="password"
                  autoComplete="new-password"
                  value={draft.api_key}
                  onChange={(e) => setDraft({ ...draft, api_key: e.target.value })}
                  placeholder={
                    draft.id && providers?.find((p) => p.id === draft.id)?.has_api_key
                      ? "•••••••• (unchanged)"
                      : "sk-…"
                  }
                  className={SETTINGS_INPUT}
                />
              </ConnField>
            </div>
            {typeInfo?.windowNeeded ? (
              <>
                <ConnField label="Context window (tokens: the smallest limit among the models this provider serves; needed to enable)">
                  <input
                    id="providerContextWindow"
                    inputMode="numeric"
                    value={draft.context_window}
                    onChange={(e) => setDraft({ ...draft, context_window: e.target.value })}
                    placeholder={typeInfo.windowPlaceholder}
                    aria-describedby={
                      contextWindowWarning(draft.context_window) ? "providerContextWindowWarning" : undefined
                    }
                    className={SETTINGS_INPUT}
                  />
                </ConnField>
                {contextWindowWarning(draft.context_window) ? (
                  <p
                    id="providerContextWindowWarning"
                    role="status"
                    data-testid="provider-small-window"
                    className="-mt-1 mb-0 text-[0.73rem] text-[var(--color-text-muted)]"
                  >
                    {contextWindowWarning(draft.context_window)}
                  </p>
                ) : null}
              </>
            ) : null}
            <ConnField
              label={
                <>
                  Models (one slug per line — these appear in everyone’s model picker as{" "}
                  <code>{draft.name.trim() || "name"}/model</code>; leave empty to serve any model
                  as a catch-all)
                </>
              }
            >
              <textarea
                id="providerModels"
                value={draft.models}
                onChange={(e) => setDraft({ ...draft, models: e.target.value })}
                placeholder={"claude-sonnet-4-5\nclaude-opus-4-8"}
                // `!` on same-property overrides of SETTINGS_INPUT (Tailwind
                // v4 orders utilities by value, not by class-string position).
                className={`${SETTINGS_INPUT} min-h-[5.6rem]! resize-y pt-[0.55rem]! font-[family-name:var(--font-code)] text-[0.76rem]! leading-[1.6]`}
              />
            </ConnField>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <label
                className="flex cursor-pointer items-center gap-[0.45rem] text-[0.875rem] text-[var(--color-text-secondary)]"
                htmlFor="providerEnabled"
              >
                <input
                  id="providerEnabled"
                  type="checkbox"
                  checked={draft.enabled}
                  onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
                  className="size-4 accent-[var(--color-primary)]"
                />
                Enabled
              </label>
              <span className="inline-flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => {
                    setDraft(null);
                    setError(null);
                    setCatchAllArmed(false);
                  }}
                  disabled={busy}
                  className={btnClass({ sm: true, reveal: true })}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  onClick={save}
                  onBlur={() => setCatchAllArmed(false)}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") setCatchAllArmed(false);
                  }}
                  disabled={busy || !draft.name.trim()}
                  data-testid="provider-save"
                  className={btnClass({ variant: "primary" })}
                >
                  {busy
                    ? "Saving…"
                    : catchAllArmed
                      ? "Confirm catch-all"
                      : draft.id === ""
                        ? "Create provider"
                        : "Save changes"}
                </button>
              </span>
            </div>
            {catchAllArmed ? (
              <p
                className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]"
                role="alert"
                data-testid="provider-catch-all-warning"
              >
                No models listed — this provider becomes a CATCH-ALL and may serve any model not
                claimed by another provider. Click again to continue.
              </p>
            ) : null}
          </div>
        ) : null}

        {providers === null ? (
          // Still loading — unless the load already failed, in which case the
          // banner + Retry above is the whole story.
          error ? null : (
            <p className="py-2 text-[0.8rem] text-[var(--color-text-muted)]">Loading…</p>
          )
        ) : providers.length === 0 && !draft ? (
          <ConnEmpty>
            No extra providers configured — the deployment is using its bundle/environment
            provider (typically OpenRouter). Add one to bring your own Anthropic/OpenAI key or a
            local endpoint.
          </ConnEmpty>
        ) : (
          <ConnRows>
            {providers.map((p) => {
              const probe = probes[p.id];
              const typeLabel = PROVIDER_TYPES[p.type]?.label ?? p.type;
              const modelsPart =
                p.models.length > 0
                  ? `${p.models.length} model${p.models.length === 1 ? "" : "s"}`
                  : "catch-all (any model)";
              // A row with no declared window cannot be enabled (rows saved
              // before the field existed may still be): flag it so the admin
              // knows what to edit.
              const windowUsed = PROVIDER_TYPES[p.type]?.windowNeeded === true;
              const windowPart =
                windowUsed && p.context_window_tokens > 0
                  ? ` · ${p.context_window_tokens.toLocaleString()}-token context`
                  : "";
              return (
                <ConnRow
                  key={p.id}
                  name={
                    <>
                      {p.name}
                      {!p.has_api_key && PROVIDER_TYPES[p.type]?.keyNeeded ? (
                        <ConnBadge variant="warn" className="ml-2">
                          No key
                        </ConnBadge>
                      ) : null}
                      {windowUsed && p.context_window_tokens <= 0 ? (
                        <ConnBadge variant="warn" className="ml-2">
                          No context window
                        </ConnBadge>
                      ) : null}
                    </>
                  }
                  sub={`${typeLabel} · ${modelsPart}${windowPart}${p.enabled ? "" : " · disabled"}`}
                  actions={
                    <>
                      <button
                        type="button"
                        onClick={() => test(p)}
                        disabled={busy || probe === "running"}
                        title="One authenticated call against the provider's endpoint — verifies the key/base URL and cross-checks the listed models."
                        className={btnClass({ sm: true, reveal: true })}
                      >
                        Test
                      </button>
                      <button
                        type="button"
                        onClick={() => toggle(p)}
                        disabled={busy}
                        className={btnClass({ sm: true, reveal: true })}
                      >
                        {p.enabled ? "Disable" : "Enable"}
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setError(null);
                          setDraft(draftFrom(p));
                        }}
                        disabled={busy}
                        className={btnClass({ sm: true, reveal: true })}
                      >
                        Edit
                      </button>
                      <InlineConfirmButton
                        label="Remove"
                        confirmLabel="Confirm remove"
                        onConfirm={() => remove(p)}
                        disabled={busy}
                      />
                    </>
                  }
                >
                  {probe === "running" ? (
                    <span className="mt-[0.1rem]">
                      <ActStatus state="running">Testing against the provider’s endpoint…</ActStatus>
                    </span>
                  ) : probe ? (
                    <span className="mt-[0.1rem]" data-testid={`probe-result-${p.id}`}>
                      <ActStatus state={probe.ok ? "ok" : "err"}>
                        {probe.ok ? "✓" : "✗"} {probe.detail}
                        {probe.latency_ms > 0 ? ` · ${probe.latency_ms}ms` : ""}
                        {(probe.missing_models?.length ?? 0) > 0
                          ? ` — missing: ${probe.missing_models!.join(", ")}`
                          : ""}
                      </ActStatus>
                    </span>
                  ) : null}
                </ConnRow>
              );
            })}
          </ConnRows>
        )}
      </ConnPanel>
    </SetSection>
  );
}
