// useChatModelCatalog — the chat composer's model-picker state (#1960), moved
// out of chat-experience.tsx unchanged: the ranked / catalog /
// workspace-provider model lists, the picker's open + search state, their
// loaders, and filteredRankedModels (the picker's ordered rows, with the
// tier, operator-pin, routing and lockdown rules).
//
// The /api/model-catalog read goes through shared/lib/models.ts's cached,
// de-duplicated loadCatalogModels — the same client the task form's picker
// uses — so the endpoint has one client and one normaliser. Its PickerModel
// rows are mapped to this module's RankedModel shape (slug for id, undefined
// for a missing number).
//
// The workspace-provider list is loaded by chat-experience's mount effect,
// which also installs the routing table, so it stays there (with the other
// hoisted mount effects, in their original order) and writes the list
// through setWorkspaceModels.

import type { Dispatch, SetStateAction } from "react";
import { useCallback, useMemo, useRef, useState } from "react";
import {
  currentPinnedModels,
  currentTierModels,
  labelForModel,
  pinnedModelFor,
  pinnedPrices,
} from "@/app/lib/modelAliases";
import type { UseClientConfig } from "@/app/lib/useClientConfig";
import { loadCatalogModels as loadSharedCatalogModels, type PickerModel } from "@/app/shared/lib/models";
import { catalogModelRoutes, modelIsAvailable, type ModelRouting } from "@/app/shared/lib/modelRouting";
import type { RankedModel } from "./chat-experience";

export type ChatModelCatalogOptions = {
  modelRouting: ModelRouting;
  workspaceModelTiers: UseClientConfig["models"];
  isLockdown: boolean;
  lockdownAllowedModels: string[];
};

export type ChatModelCatalog = {
  rankedModels: RankedModel[];
  catalogModels: RankedModel[];
  workspaceModels: RankedModel[];
  setWorkspaceModels: Dispatch<SetStateAction<RankedModel[]>>;
  modelPickerOpen: boolean;
  setModelPickerOpen: Dispatch<SetStateAction<boolean>>;
  modelSearchQuery: string;
  setModelSearchQuery: Dispatch<SetStateAction<string>>;
  isLoadingRankedModels: boolean;
  isLoadingCatalog: boolean;
  filteredRankedModels: RankedModel[];
  loadRankedModels: () => Promise<void>;
  loadCatalogModels: () => Promise<void>;
};

// The loaders as the components call them (fire-and-forget).
type ModelPickerLoaders = {
  loadRankedModels: () => void | Promise<void>;
  loadCatalogModels: () => void | Promise<void>;
};

/** What the composer's model picker reads and drives. */
export type ComposerModelCatalog = Pick<
  ChatModelCatalog,
  | "modelPickerOpen"
  | "setModelPickerOpen"
  | "modelSearchQuery"
  | "setModelSearchQuery"
  | "filteredRankedModels"
  | "isLoadingRankedModels"
  | "isLoadingCatalog"
> &
  ModelPickerLoaders;

/** What the transcript needs to open the picker (the model-switch card). */
export type TranscriptModelCatalog = Pick<
  ChatModelCatalog,
  "setModelPickerOpen" | "setModelSearchQuery"
> &
  ModelPickerLoaders;

// pickerToRanked maps a shared catalog row onto the chat's RankedModel. The
// shared normaliser reports a missing number as null; RankedModel's fields
// are optional, so null becomes undefined.
export function pickerToRanked(m: PickerModel): RankedModel {
  return {
    slug: m.id,
    name: m.name,
    contextLength: m.contextLength ?? undefined,
    created: m.created ?? undefined,
    pricePrompt: m.pricePrompt ?? undefined,
    priceCompletion: m.priceCompletion ?? undefined,
  };
}

const MAX_SEARCH_RESULTS = 15;

export function useChatModelCatalog({
  modelRouting,
  workspaceModelTiers,
  isLockdown,
  lockdownAllowedModels,
}: ChatModelCatalogOptions): ChatModelCatalog {
  const [rankedModels, setRankedModels] = useState<RankedModel[]>([]);
  const [catalogModels, setCatalogModels] = useState<RankedModel[]>([]);
  // Active workspace-provider models, "<provider>/<model>" slugs. Loaded via the
  // shared lib (which also expands catch-all anthropic/openai providers from
  // the catwalk catalog); empty when none are configured or the fetch fails.
  const [workspaceModels, setWorkspaceModels] = useState<RankedModel[]>([]);
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [modelSearchQuery, setModelSearchQuery] = useState<string>("");
  const [isLoadingRankedModels, setIsLoadingRankedModels] = useState(false);
  const [isLoadingCatalog, setIsLoadingCatalog] = useState(false);

  // With no query, show "default" + "advanced" + the top-ranked list. As
  // soon as the user types, expand the search to the full budget-filtered
  // catalog so they can find cheaper models without leaving the page.
  // Catalog order is price-descending (up to the $30/M ceiling) so the
  // best-quality options show first. Results are capped so the dropdown
  // stays short (MAX_SEARCH_RESULTS).
  //
  // Tier slugs deliberately appear twice in the no-query view: once as
  // the friendly alias row at the top (`default` / `advanced`) and once
  // under their lab in the rankings section
  // (`google/gemini-3-flash-preview`, …). Both rows select the same
  // model — users searching by lab or model name shouldn't have to know
  // that "advanced" maps to Claude Sonnet.
  const filteredRankedModels = useMemo(() => {
    const query = modelSearchQuery.trim().toLowerCase();
    // Operator pins (FLEET_PINNED_MODELS) relabel their slug everywhere the
    // picker names it, so the catalog/ranked rows are renamed up front. The
    // pin list is module state installed by the same /api/client-config
    // payload that replaces workspaceModelTiers, so that dep re-runs this.
    const relabel = (model: RankedModel): RankedModel => {
      const pin = pinnedModelFor(model.slug);
      if (!pin) return model;
      return {
        ...model,
        ...(pin.label ? { name: pin.label, relabelled: true } : {}),
        ...(pin.tag ? { pinTag: pin.tag } : {}),
        ...pinnedPrices(pin),
      };
    };
    const ranked = rankedModels.map(relabel);
    const catalog = catalogModels.map(relabel);
    // The two pinned rows are hand-written, so their prices have to be joined
    // back from the catalog (or the ranked list when the catalog is empty) —
    // otherwise the recommended models would be the only rows in the listbox
    // without a cost indicator.
    const pricesFor = (slug: string) => {
      const hit =
        catalog.find((m) => m.slug === slug) ??
        ranked.find((m) => m.slug === slug);
      if (!hit) return {};
      return {
        pricePrompt: hit.pricePrompt,
        priceCompletion: hit.priceCompletion,
      };
    };
    const tierSlugs = workspaceModelTiers
      ? [workspaceModelTiers.defaultModel, workspaceModelTiers.advancedModel]
      : currentTierModels().map((tier) => tier.slug);
    const tierModels: RankedModel[] = tierSlugs.map((slug) => ({
      slug,
      name: labelForModel(slug),
      ...pricesFor(slug),
    }));
    const defaults = tierModels.filter((model) =>
      modelIsAvailable(model.slug, modelRouting, true),
    );
    // Pinned rows sit directly under the recommended tiers. A pin with no
    // label keeps the catalog (or workspace-provider) display name; that row
    // also supplies the prices / context / created / workspace flag a
    // hand-written row would otherwise lack.
    const pinnedSeeds: RankedModel[] = currentPinnedModels().map((pin) => {
      const hit =
        catalog.find((m) => m.slug === pin.slug) ??
        ranked.find((m) => m.slug === pin.slug) ??
        workspaceModels.find((m) => m.slug === pin.slug);
      return relabel({
        ...(hit ?? {}),
        slug: pin.slug,
        name: hit?.name ?? pin.slug,
      });
    });
    const publicCatalog = new Set(
      [...catalog, ...ranked].map((model) => model.slug),
    );
    const catalogChoices = (models: RankedModel[]): RankedModel[] =>
      models.flatMap((model) =>
        catalogModelRoutes(
          model.slug,
          modelRouting,
          publicCatalog.has(model.slug),
        ).map((route) => ({
          ...model,
          slug: route.slug,
          name: route.provider
            ? `${route.provider}: ${model.name}`
            : model.name,
          workspace: !!route.provider,
        })),
      );
    // Public pins take the same explicit-route rewrite as catalog rows (a
    // native catch-all can shadow the OpenRouter one); workspace pins are
    // already provider-routed and only need the availability check.
    const pinnedModels: RankedModel[] = [
      ...pinnedSeeds.filter((m) => m.workspace),
      ...catalogChoices(pinnedSeeds.filter((m) => !m.workspace)),
    ].filter((m) => modelIsAvailable(m.slug, modelRouting, !m.workspace));

    // Lockdown chats are pinned to the operator-configured allow-list.
    // Build a fixed list that mirrors that allow-list (default first,
    // others in declared order), ignore the freeform search query, and
    // skip the catalog entirely. Server-side enforcement is the source
    // of truth; this filter is just the UX so the user doesn't pick a
    // slug we'd reject anyway.
    if (isLockdown) {
      const allowed = lockdownAllowedModels;
      if (allowed.length === 0) return defaults;
      const seen = new Set<string>();
      const out: RankedModel[] = [];
      for (const slug of allowed) {
        if (!modelIsAvailable(slug, modelRouting)) continue;
        if (seen.has(slug)) continue;
        seen.add(slug);
        const aliased = defaults.find((d) => d.slug === slug);
        if (aliased) {
          out.push(aliased);
          continue;
        }
        const fromRanked = ranked.find((m) => m.slug === slug);
        const fromCatalog = catalog.find((m) => m.slug === slug);
        const known = fromRanked ?? fromCatalog;
        out.push(
          known ??
            pinnedSeeds.find((m) => m.slug === slug) ?? { slug, name: slug },
        );
      }
      return out;
    }

    if (!query) {
      // Browse order: pinned defaults, then workspace-provider models (the
      // operator configured them deliberately — they outrank the generic
      // rankings), then the ranked list.
      const seen = new Set<string>();
      const out: RankedModel[] = [];
      for (const m of [
        ...defaults,
        ...pinnedModels,
        ...workspaceModels,
        ...catalogChoices([...tierModels, ...ranked]),
      ]) {
        if (!modelIsAvailable(m.slug, modelRouting, !m.workspace)) continue;
        if (seen.has(m.slug)) continue;
        seen.add(m.slug);
        out.push(m);
      }
      return out;
    }
    const source = catalogChoices(
      catalog.length > 0 ? catalog : ranked,
    );
    // A relabelled pin matches on its label only: searching the underlying
    // slug's vendor/model name should not surface the renamed row.
    const matchesQuery = (m: RankedModel) =>
      (!m.relabelled && m.slug.toLowerCase().includes(query)) ||
      m.name.toLowerCase().includes(query);
    const seen = new Set<string>();
    const matches: RankedModel[] = [];
    for (const d of [
      ...defaults,
      ...pinnedModels,
      ...workspaceModels,
      ...catalogChoices(tierModels),
    ]) {
      if (seen.has(d.slug)) continue;
      if (matchesQuery(d)) {
        seen.add(d.slug);
        matches.push(d);
      }
    }
    for (const model of source) {
      if (!modelIsAvailable(model.slug, modelRouting, true)) continue;
      if (seen.has(model.slug)) continue;
      if (!matchesQuery(model)) continue;
      seen.add(model.slug);
      matches.push(model);
      if (matches.length >= MAX_SEARCH_RESULTS) break;
    }
    return matches;
  }, [
    rankedModels,
    catalogModels,
    workspaceModels,
    modelRouting,
    workspaceModelTiers,
    modelSearchQuery,
    isLockdown,
    lockdownAllowedModels,
  ]);

  const loadRankedModels = async () => {
    if (rankedModels.length > 0 || isLoadingRankedModels) return;
    setIsLoadingRankedModels(true);
    try {
      const response = await fetch("/api/model-rankings", {
        cache: "no-store",
      });
      if (!response.ok) return;
      const data = (await response.json()) as {
        models?: Array<{
          slug: string;
          name: string;
          created?: number;
          price_prompt?: number;
          price_completion?: number;
        }>;
      };
      setRankedModels(
        (data.models ?? []).map((m) => ({
          slug: m.slug,
          name: m.name,
          created: m.created,
          pricePrompt: m.price_prompt,
          priceCompletion: m.price_completion,
        })),
      );
    } catch {
      /* optional enhancement only */
    } finally {
      setIsLoadingRankedModels(false);
    }
  };

  // loadCatalogModels pulls the full budget-filtered list of text models
  // from OpenRouter. Larger than the ranked list but still small (~a few
  // hundred entries), so we fetch once per session and search client-side.
  // The response carries `context_length` per model — used by the
  // context-usage indicator (Show details) as well as the picker. The
  // request itself is shared/lib/models.ts's cached, de-duplicated
  // loadCatalogModels, so a catalog the task form already read is reused.
  //
  // Wrapped in useCallback so its identity only changes when the dedup
  // inputs (catalogModels.length / isLoadingCatalog) actually change — the
  // hoisted mount effect lists it as a dependency, so a fresh-every-render
  // identity would re-fire that effect each render. The early-return guard
  // makes any re-fire after the first successful load a no-op.
  //
  // catalogAttemptedRef makes it once-per-session regardless of OUTCOME: with
  // only the length/loading guards, an EMPTY or failing catalog left length
  // at 0 while isLoadingCatalog's flip cycled the callback identity — the
  // mount effect re-fired in a tight loop and the client hammered
  // /api/model-catalog indefinitely (thousands of requests in seconds,
  // starving the UI; found via a Playwright trace in the mocked suite, but
  // any deployment whose catalog errors would self-DDoS the same way).
  const catalogAttemptedRef = useRef(false);
  const loadCatalogModels = useCallback(async () => {
    if (
      catalogAttemptedRef.current ||
      catalogModels.length > 0 ||
      isLoadingCatalog
    )
      return;
    catalogAttemptedRef.current = true;
    setIsLoadingCatalog(true);
    try {
      const models = await loadSharedCatalogModels();
      setCatalogModels(models.map(pickerToRanked));
    } catch {
      /* autocomplete enhancement only */
    } finally {
      setIsLoadingCatalog(false);
    }
  }, [catalogModels.length, isLoadingCatalog]);

  return {
    rankedModels,
    catalogModels,
    workspaceModels,
    setWorkspaceModels,
    modelPickerOpen,
    setModelPickerOpen,
    modelSearchQuery,
    setModelSearchQuery,
    isLoadingRankedModels,
    isLoadingCatalog,
    filteredRankedModels,
    loadRankedModels,
    loadCatalogModels,
  };
}
