# Stale clients after a deploy: the update notice and the error pages

Every merge to `main` is a release, so a fleet deployment can change several
times a day while people keep a tab open. This page records how the web app
copes with a tab that is running an older bundle than the server: what shipped,
what was considered and left out, and why.

## The problem it was written for

An iPhone or iPad **Home Screen web app** (Safari's "Add to Home Screen"; on iOS
26 every site added there opens this way) is the extreme case:

- iOS freezes the app when it goes to the background and resumes it later, so
  the JavaScript it runs can be days old.
- It has no address bar and no reload button. If the page breaks, the only way
  out the platform offers is force-quitting the app from the app switcher.

Before this change:

- Only `/chat` checked for a new deploy. The only sign of one was a button in
  the chat sidebar's footer, which on a phone sits inside a closed drawer. The
  Operations Center, Settings and Guides never checked.
- The app had no `error.tsx` or `global-error.tsx`. A client crash showed Next's
  bare "Application error: a client-side exception has occurred" text with no
  control on the page.

## What shipped

- **One deploy probe per tab, app-wide.** `web/src/app/shared/deployDrift.ts`
  compares the bundle's baked-in build id (`VITE_BUILD_ID`, the git SHA the
  bundle was built from — the same id Go stamps as `X-App-Version`) with
  `GET /api/version`. It probes:
  - on load;
  - when the tab becomes visible, which is the event a resumed Home Screen app
    fires;
  - on `focus` and `online`;
  - every 5 minutes while the tab is visible.

  Overlapping probes share one request, and probing stops once drift is known.
  It is skipped when no build id is baked in (a dev server).
- **The update notice.** `web/src/app/shared/UpdateNotice.tsx` is mounted once
  from the root (`App.tsx`). When the build differs it shows a compact notice at the
  top of the page: "*{app name}* was updated", **Reload**, and **×**.
  - It uses the bundle's app name.
  - It respects the iOS safe area (the app uses the black-translucent status
    bar) and sits above page chrome but below dialogs.
  - It is skipped on the signed-out pages (`/login`, `/no-access`, `/shared/*`),
    where `/api/version` answers 401 or the viewer is anonymous.
  - The chat sidebar button it replaces is gone, and chat no longer probes on
    its own. It still refreshes its client config on the same events.
- **Error pages.** One error boundary at the root of the SPA
  (`ErrorBoundary` in `web/src/App.tsx`, inside the themed document, so it keeps the
  deployment's palette; it resets on navigation) renders
  `shared/ui/ErrorScreen.tsx`, which shows:
  - **Reload**;
  - **Go to the home page** (a hard navigation, since the client router may be
    what failed);
  - the error's digest, when there is one.
- **One automatic reload for a failed chunk load.** `shared/chunkReload.ts`
  recognizes a failed code-split load in each shape it arrives in: the
  bundlers' `ChunkLoadError`, and Chromium, WebKit and Firefox's
  dynamic-import messages (what a lazy route chunk of the Vite bundle throws). The error screen then reloads by itself, once.
  - A timestamp in session storage stops a second automatic reload within two
    minutes, so a page that fails the same way again waits for the reader
    instead of looping.
  - When session storage is unavailable it never reloads by itself.

## Deliberately not done

- **No automatic reload on drift.** An earlier version of chat's probe reloaded
  the page when the build changed. It surfaced as "every click jumps me to the
  top", because the reload fired in the middle of an interaction. The notice
  leaves the timing to the reader. The one automatic reload kept is for a page
  that has already failed.
- **No reload on client-side navigation.** The Next.js tier this replaced
  compared build ids on every client-side navigation and hard-loaded on a
  mismatch; the SPA's in-house router does not (ADR-0091). A stale tab keeps
  working until it needs a chunk that is gone — which the one automatic reload
  above covers — and the update notice tells the reader in the meantime.
  Content-hashed `/assets/*` are served `immutable`, and HTML `no-store`, so a
  reload always gets the current build.
- **No service-worker caching.** `public/sw.js` stays push-only with no fetch
  handler, so it can never serve a stale shell.

## Tests

- `shared/deployDrift.test.ts`: drift, quiet cases (same build, failed probe,
  a body with no build id, a dev build), one shared request, and the
  event/interval/visibility rules.
- `shared/UpdateNotice.test.tsx`: shows on a non-chat page with the app name,
  reloads, dismisses, stays off on signed-out pages, and pins chat no longer
  probing.
- `shared/chunkReload.test.ts` and `shared/ui/ErrorScreen.test.tsx`: error
  recognition, a single automatic reload with its loop guard, and the buttons.
- `e2e/mocked/update-notice.spec.ts`, at a phone viewport: the notice on
  Settings fits the width and dismisses; chat shows it outside the drawer; no
  notice when nothing changed.
