/**
 * proxyHeaders — the response headers a proxy funnel forwards upstream → browser.
 *
 * Both funnels (`chatServerPassthrough` here, `proxyToOrchestrator` under
 * api/orchestrator/_lib) used to re-emit only `Content-Type` and drop everything
 * else. That silently defeated `Content-Disposition`, which the Go handlers set
 * deliberately on their download endpoints:
 *
 *   internal/sched/handlers/datasets.go   attachment; filename="<dataset>.csv"
 *   internal/sched/handlers/prompts.go    attachment; filename="fleet-prompts.json"
 *   internal/sched/handlers/adoption.go   attachment; filename="fleet-adoption-<date>.csv"
 *   internal/httpapi/projects.go          attachment; filename="<project>-<id>.json"
 *
 * With the header gone, `<a href="…/export" download>` fell back to deriving the
 * name from the URL's last path segment — so "Export CSV" saved a file literally
 * named `export`, with no extension, which opens in nothing. The whole point of
 * `sanitizeDatasetFilename` / `exportFilename` never reached the user.
 *
 * The fix belongs here rather than in each route: a passthrough concern fixed
 * once. `api/conversations/[id]/export` already got this right by hand and its
 * comment says why — "so the browser gets the Content-Disposition filename the
 * Go server chose". This generalizes that.
 */

/**
 * Headers copied from the upstream response.
 *
 * `Content-Length` is deliberately NOT forwarded. `fetch()` transparently
 * decodes a compressed upstream body, so the upstream length can describe a
 * different number of bytes than the ones we re-emit; a wrong Content-Length
 * truncates the response. Letting the runtime compute it is always correct.
 * (`orchestrator/tasks/[taskId]/workspace/[...path]` does forward it — safe only
 * because nothing in that path negotiates encoding today.)
 */
const FORWARDED = ["Content-Type", "Content-Disposition", "Cache-Control", "Last-Modified", "ETag"];

/**
 * forwardedHeaders builds the browser-facing headers for a proxied response,
 * copying the allowlist above and defaulting Content-Type when upstream omits it
 * (matching the previous behavior of both funnels).
 */
export function forwardedHeaders(
  upstream: Response,
  fallbackContentType = "application/json",
): Headers {
  const headers = new Headers();
  for (const name of FORWARDED) {
    const value = upstream.headers.get(name);
    if (value) headers.set(name, value);
  }
  if (!headers.has("Content-Type")) headers.set("Content-Type", fallbackContentType);
  return headers;
}
