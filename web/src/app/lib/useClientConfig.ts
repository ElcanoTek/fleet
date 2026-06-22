"use client";

import { useEffect, useState } from "react";
import { DEFAULT_PILLS, type ProtocolPill } from "@/app/chat/ui/protocolPills";
import { type RuntimeFlavor } from "@/app/chat/ui/RuntimePicker";

// useClientConfig fetches the active client's runtime config from
// /api/client-config (which proxies chat-server's member-gated /client-config)
// so the chat UI renders client-agnostic branding + empty-state pills instead
// of hardcoded strings. It falls back to neutral defaults on error / while
// loading, so the experience is never blank and never client-specific by
// accident.
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
  // runtimes is the bundle's runtime-flavor catalog for the chat flavor picker;
  // defaultRuntime is the flavor a conversation uses when it has no explicit
  // choice. Empty/single-flavor catalogs let the picker hide itself.
  runtimes: RuntimeFlavor[];
  defaultRuntime: string;
  loading: boolean;
};

type ClientConfigResponse = {
  branding?: Partial<ClientBranding>;
  empty_state?: { cards?: ProtocolPill[] };
  runtimes?: RuntimeFlavor[];
  default_runtime?: string;
};

export function useClientConfig(enabled = true): UseClientConfig {
  const [branding, setBranding] = useState<ClientBranding>(DEFAULT_BRANDING);
  const [pills, setPills] = useState<ProtocolPill[]>(DEFAULT_PILLS);
  const [runtimes, setRuntimes] = useState<RuntimeFlavor[]>([]);
  const [defaultRuntime, setDefaultRuntime] = useState<string>("");
  const [loading, setLoading] = useState(enabled);

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
        setBranding({ ...DEFAULT_BRANDING, ...(data.branding ?? {}) });
        const cards = data.empty_state?.cards;
        setPills(Array.isArray(cards) && cards.length > 0 ? cards : DEFAULT_PILLS);
        setRuntimes(Array.isArray(data.runtimes) ? data.runtimes : []);
        setDefaultRuntime(typeof data.default_runtime === "string" ? data.default_runtime : "");
      } catch {
        if (cancelled) return;
        // Fall back to neutral defaults — never blank, never client-specific.
        setBranding(DEFAULT_BRANDING);
        setPills(DEFAULT_PILLS);
        setRuntimes([]);
        setDefaultRuntime("");
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [enabled]);

  return { branding, pills, runtimes, defaultRuntime, loading };
}
