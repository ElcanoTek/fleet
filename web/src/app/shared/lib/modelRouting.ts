// Public metadata only. A missing snapshot (older server / failed read) is
// unknown, not an empty routing table. Execution remains the backend's authority.
export type ModelProvider = {
  name: string;
  type: string;
  models: string[];
  catch_all: boolean;
};

export type ModelRouting = ModelProvider[] | null;

// Match the resolver's precedence. Catalog suggestions must not treat a native
// catch-all as an OpenRouter gateway: OpenAI cannot serve a Google catalog slug.
// Explicit routes and listed models remain valid for custom gateways.
export function modelIsAvailable(slug: string, providers: ModelRouting, catalog = false): boolean {
  if (providers === null) return true;
  const value = slug.trim();
  if (!value) return false;
  const slash = value.indexOf("/");
  if (slash > 0 && value.slice(slash + 1).trim() && providers.some((p) => p.name === value.slice(0, slash))) {
    return true;
  }
  if (providers.some((p) => p.models.includes(value))) return true;
  const fallback = providers.find((p) => p.catch_all);
  return !!fallback && (!catalog || fallback.type === "openrouter");
}

export function unavailableModelMessage(slug: string): string {
  return `Model "${slug}" is not available through this workspace's configured providers. Choose a workspace model in the model picker, or ask an admin to configure its provider in Settings → Admin → Model providers.`;
}
