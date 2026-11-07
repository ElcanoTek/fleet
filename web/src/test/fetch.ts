// Test helpers for code that calls fetch (#2030).
//
// Tests used to hand-build `{ ok, status, json }` objects. Those drift from a
// real Response: `ok` disagreeing with `status`, a `json()` that succeeds on an
// empty error body, no `text()` or `headers` at all — so the code under test
// was exercised against a shape the browser never produces. These build real
// `Response` objects instead.
//
// vitest.config.ts sets `unstubGlobals: true`, so a fetch stubbed here is
// removed after each test without an afterEach.

import { vi, type Mock } from "vitest";

// Statuses whose Response may not carry a body (the constructor throws).
const NULL_BODY_STATUSES = new Set([101, 204, 205, 304]);

/** A JSON Response: `body` serialized, with a JSON Content-Type. */
export function jsonResponse(body: unknown, status = 200, init: ResponseInit = {}): Response {
  if (NULL_BODY_STATUSES.has(status)) return new Response(null, { ...init, status });
  const headers = new Headers(init.headers);
  if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  return new Response(JSON.stringify(body), { ...init, status, headers });
}

/** A plain-text Response — how the Go handlers send a refusal's reason. */
export function textResponse(text: string, status = 200, init: ResponseInit = {}): Response {
  if (NULL_BODY_STATUSES.has(status)) return new Response(null, { ...init, status });
  return new Response(text, { ...init, status });
}

export type FetchImpl = (url: string, init?: RequestInit) => Response | Promise<Response>;

/**
 * A route's answer: a Response (cloned per call, so a route can be read more
 * than once), a function of the request, or any other value — sent as a 200
 * JSON body.
 */
export type Route = Response | ((url: string, init?: RequestInit) => unknown) | object | string | number | boolean | null;

function toResponse(value: unknown): Response {
  return value instanceof Response ? value.clone() : jsonResponse(value);
}

/**
 * stubFetch replaces the global fetch for this test and returns the mock.
 *
 * - `stubFetch(impl)`: every call goes to `impl(url, init)`.
 * - `stubFetch(routes)`: the key `"METHOD /path"` (e.g. `"POST /api/x"`) is
 *   tried first, then the bare URL `"/api/x"`; the full URL (query included)
 *   is matched before the path alone. A function route's return value goes
 *   through the same rules (a Response, or a value sent as JSON). An
 *   unmatched request answers 404 `{}`.
 */
export function stubFetch(routes: Record<string, Route> | FetchImpl): Mock<FetchImpl> {
  const impl: FetchImpl =
    typeof routes === "function"
      ? routes
      : async (url, init) => {
          const method = (init?.method ?? "GET").toUpperCase();
          const path = url.split("?")[0];
          for (const key of [`${method} ${url}`, url, `${method} ${path}`, path]) {
            if (Object.prototype.hasOwnProperty.call(routes, key)) {
              const route = routes[key];
              return toResponse(typeof route === "function" ? await route(url, init) : route);
            }
          }
          return jsonResponse({}, 404);
        };
  const mock = vi.fn((input: RequestInfo | URL, init?: RequestInit) =>
    Promise.resolve(impl(typeof input === "string" ? input : input instanceof URL ? input.href : input.url, init)),
  );
  vi.stubGlobal("fetch", mock);
  return mock as unknown as Mock<FetchImpl>;
}
