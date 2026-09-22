/**
 * Response headers for an SSE proxy route.
 *
 * Both SSE proxies (`/api/chat` and `/api/conversations/[id]/stream`) REBUILD
 * the response header set rather than passing chat-server's through, so every
 * header the browser needs has to be listed explicitly. That is deliberate —
 * it keeps an upstream header from leaking to the browser by accident — but it
 * is also a standing trap: a header added server-side simply never arrives, and
 * nothing fails loudly when it doesn't.
 *
 * It had already happened. chat-server advertises its keepalive cadence on
 * `X-Fleet-Heartbeat-Interval-Ms` (internal/httpapi/capabilities.go), and
 * useTurnStream reads it to size the stream-liveness watchdog. Neither proxy
 * forwarded it, so in any deployment that goes through Next the client read
 * absent → 0 → "keepalives are off, silence proves nothing", which pins
 * `streamDeadSilenceMs` at Infinity and disables the missed-keepalive branch of
 * `checkStreamLiveness` entirely. The watchdog degraded to its no-cadence
 * fallback everywhere and looked like it was working.
 *
 * So the pass-through list lives here, once, and both routes use it.
 */

/** Headers chat-server sets on an SSE response that the browser must receive. */
const passthroughHeaders = [
  // The keepalive cadence the attached stream promises; sizes the liveness
  // watchdog's silence thresholds (#1584).
  "X-Fleet-Heartbeat-Interval-Ms",
  // The conversation this stream belongs to. A brand-new chat posts under a
  // client-side pending key, and if the socket dies before the `conversation`
  // frame this header is the only id it ever learns (#1591).
  "X-Fleet-Conversation-Id",
] as const;

/**
 * Build the SSE response headers for a proxy route: the fixed streaming set,
 * plus every pass-through header chat-server actually sent.
 */
export function sseProxyHeaders(upstream: Response): Record<string, string> {
  const headers: Record<string, string> = {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache, no-transform",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  };
  for (const name of passthroughHeaders) {
    const value = upstream.headers.get(name);
    if (value !== null) headers[name] = value;
  }
  return headers;
}

/** Exported for the tests that pin the list against the routes. */
export const ssePassthroughHeaderNames: readonly string[] = passthroughHeaders;
