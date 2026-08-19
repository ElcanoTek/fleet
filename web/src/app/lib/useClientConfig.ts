"use client";

import { useEffect, useState } from "react";
import { DEFAULT_PILLS, type ProtocolPill } from "@/app/chat/ui/protocolPills";
import {
  currentAdvancedModel,
  currentDefaultModel,
  setModelTiers,
  type ModelTiersConfig,
} from "@/app/lib/modelAliases";

// useClientConfig fetches the active client's runtime config from
// /api/client-config (which proxies chat-server's member-gated /client-config)
// so the chat UI renders client-agnostic branding + empty-state pills instead
// of hardcoded strings. It falls back to neutral defaults on error / while
// loading, so the experience is never blank and never client-specific by
// accident. The last successful payload is cached at module scope so a
// remount (crossing between /chat and /orchestrator) re-renders the real
// branding immediately instead of flashing the defaults.
//
// The endpoint is member-gated, so this hook is only ever called from
// authenticated surfaces (the chat experience). Pre-auth surfaces (the login
// card) must NOT depend on it — they use neutral hardcoded defaults instead.

const APP_NAME = process.env.NEXT_PUBLIC_APP_NAME?.trim() || "Fleet";

export type ClientBranding = {
  app_name: string;
  login_title: string;
  login_tagline: string;
  share_title: string;
  share_description: string;
  // Web path of the bundle's brand mark, or "" when it declares none (the nav
  // rail then renders fleet's own). The backend only sets this when a file
  // actually backed branding.logo at load, so an empty string means "no logo",
  // never "logo configured but broken".
  logo_url?: string;
};

// Neutral, client-agnostic branding. Mirrors config/default/manifest.yaml's
// `branding` block so the bare fleet experience matches the generic bundle.
export const DEFAULT_BRANDING: ClientBranding = {
  app_name: APP_NAME,
  login_title: "Welcome aboard.",
  login_tagline: "Sign in to your workspace and pick up where you left off.",
  share_title: `${APP_NAME} — your team's AI workspace`,
  share_description:
    "Persistent multi-turn conversations with real tool use across files, data, and the web.",
};

export type UseClientConfig = {
  branding: ClientBranding;
  pills: ProtocolPill[];
  // The workspace's effective model tiers (#1187), null until a
  // /api/client-config payload has resolved in this page's lifetime. When it
  // lands, setModelTiers() has ALREADY installed the pair module-wide — this
  // field exists so a consumer can react to the arrival itself (e.g. adopting
  // the live default for a not-yet-started conversation).
  models: { defaultModel: string; advancedModel: string } | null;
  loading: boolean;
};

type ClientConfigResponse = {
  branding?: Partial<ClientBranding>;
  empty_state?: { cards?: ProtocolPill[] };
  models?: ModelTiersConfig;
};

// The last successful /api/client-config payload, cached at module scope.
// Chat ⇄ Operations Center is ordinary route navigation (two routes, one
// rail), so crossing surfaces remounts the shell — and this hook with it.
// Without the cache every crossing restarted from the neutral defaults and
// the rail's brand row flashed the default app name until the re-fetch
// resolved; seeding state from the cache renders the last-known branding on
// the first frame instead. The fetch still runs on every mount so a
// long-lived page picks up config changes. Hydration stays consistent: the
// fetch only ever runs in a browser effect, so on the server and on any
// initial document load the cache is empty and both sides render defaults.
let cachedConfig: {
  branding: ClientBranding;
  pills: ProtocolPill[];
  models: { defaultModel: string; advancedModel: string } | null;
} | null = null;

// Module-scope reader so the useState initializers don't read a mutable
// module binding directly from the component body (react-hooks/purity —
// same pattern as nowMs in chat-experience.tsx).
const readCachedConfig = () => cachedConfig;

export function __resetClientConfigCacheForTests() {
  cachedConfig = null;
}

export function useClientConfig(enabled = true): UseClientConfig {
  const [branding, setBranding] = useState<ClientBranding>(
    () => readCachedConfig()?.branding ?? DEFAULT_BRANDING,
  );
  const [pills, setPills] = useState<ProtocolPill[]>(() => readCachedConfig()?.pills ?? DEFAULT_PILLS);
  const [models, setModels] = useState<UseClientConfig["models"]>(
    () => readCachedConfig()?.models ?? null,
  );
  const [loading, setLoading] = useState(() => enabled && readCachedConfig() === null);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;

    void (async () => {
      try {
        const res = await fetch("/api/client-config", { cache: "no-store" });
        if (!res.ok) throw new Error(`client-config ${res.status}`);
        const data = (await res.json()) as ClientConfigResponse;
        if (cancelled) return;
        // Merge over neutral defaults so a partial branding block still renders.
        const cards = data.empty_state?.cards;
        // Install the workspace tier pair module-wide BEFORE any state update,
        // so the re-render this triggers already reads the live slugs.
        setModelTiers(data.models);
        const next = {
          branding: { ...DEFAULT_BRANDING, ...(data.branding ?? {}) },
          pills: Array.isArray(cards) && cards.length > 0 ? cards : DEFAULT_PILLS,
          models: {
            defaultModel: currentDefaultModel(),
            advancedModel: currentAdvancedModel(),
          },
        };
        cachedConfig = next;
        setBranding(next.branding);
        setPills(next.pills);
        setModels(next.models);
      } catch {
        // Keep whatever the state was seeded with — the cached config on a
        // remount, the neutral defaults otherwise. Never blank, never
        // client-specific by accident.
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [enabled]);

  return { branding, pills, models, loading };
}
