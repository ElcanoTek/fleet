import { getServerBranding } from "@/app/lib/serverBranding";

// Tab titles for the guides.
//
// The root layout sets the tab title to the BUNDLE's app name, which on a
// white-labeled deployment is the whole point — the name in the tab is the
// customer's, not ours. A page-level `title` replaces that string outright
// (the root declares no title.template), so a bare `title: "Guides"` would
// make /help the one surface in the app whose tab drops the deployment's name.
//
// So the guides follow the convention the chat experience already uses for a
// named conversation — "<page> — <app name>" — and a bundle that has not set a
// name falls back to the page title alone rather than a dangling dash.
export async function guidePageTitle(page: string): Promise<string> {
  const appName = (await getServerBranding()).appName?.trim();
  return appName ? `${page} — ${appName}` : page;
}
