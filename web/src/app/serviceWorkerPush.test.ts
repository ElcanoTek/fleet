import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it, vi } from "vitest";

// public/sw.js is served as-is, not bundled, so these tests evaluate the real
// file against a fake ServiceWorkerGlobalScope and deliver push events to it.
// The server sends only { title, body?, url? } (internal/webpush); the worker
// must render exactly that, and still show something for a payload it cannot
// read.

const SOURCE = readFileSync(resolve(__dirname, "../../public/sw.js"), "utf8");
const ORIGIN = "https://fleet.example.com";

type Listener = (event: unknown) => void;

function loadWorker() {
  const listeners: Record<string, Listener> = {};
  // Each call returns its own promise, so the test can check that exactly
  // that promise was handed to event.waitUntil.
  const showNotification = vi.fn(() => Promise.resolve());
  const scope = {
    location: new URL(`${ORIGIN}/sw.js`),
    clients: {},
    skipWaiting: vi.fn(),
    registration: { showNotification },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
  };
  // In a real worker scope `clients`, `location` and `registration` are also
  // bare globals.
  new Function("self", "clients", "location", "registration", SOURCE)(
    scope,
    scope.clients,
    scope.location,
    scope.registration,
  );
  return { listeners, showNotification };
}

// push delivers one push event and returns what the worker showed. The
// display must be handed to event.waitUntil: without it the browser may stop
// the worker first, and Chrome then shows its own "updated in the background"
// notice instead.
async function push(data: { json: () => unknown } | undefined) {
  const worker = loadWorker();
  const waitUntil = vi.fn();
  worker.listeners.push({ data, waitUntil });
  expect(worker.showNotification).toHaveBeenCalledTimes(1);
  expect(waitUntil).toHaveBeenCalledWith(worker.showNotification.mock.results[0].value);
  await waitUntil.mock.calls[0][0];
  return worker.showNotification.mock.calls[0] as unknown as [string, Record<string, unknown>];
}

describe("sw.js push", () => {
  it("shows the payload's title and body, with the app icon and the link to open", async () => {
    const [title, options] = await push({
      json: () => ({
        title: "✓ Task complete: nightly report (1m30s)",
        body: "short body",
        url: `${ORIGIN}/orchestrator/tasks/t-1`,
      }),
    });
    expect(title).toBe("✓ Task complete: nightly report (1m30s)");
    expect(options).toEqual({
      body: "short body",
      icon: "/app-icons/icon-192.png",
      data: { url: `${ORIGIN}/orchestrator/tasks/t-1` },
    });
  });

  it("links to the app when the payload names no link", async () => {
    const [, options] = await push({ json: () => ({ title: "⚠ Approval needed: send_email" }) });
    expect(options.data).toEqual({ url: ORIGIN });
    expect(options.body).toBe("");
  });

  it("renders nothing but title, body and link, whatever else a payload carries", async () => {
    const [title, options] = await push({
      json: () => ({ title: "t", url: `${ORIGIN}/chat?c=1`, message: "model output", tag: "x" }),
    });
    expect(title).toBe("t");
    expect(options).toEqual({
      body: "",
      icon: "/app-icons/icon-192.png",
      data: { url: `${ORIGIN}/chat?c=1` },
    });
  });

  it("still shows a generic Fleet notification for a payload it cannot read", async () => {
    const [title, options] = await push({
      json: () => {
        throw new SyntaxError("not JSON");
      },
    });
    expect(title).toBe("Fleet");
    expect(options).toEqual({ body: "", icon: "/app-icons/icon-192.png", data: { url: ORIGIN } });
  });

  it("shows a generic Fleet notification for a push with no data", async () => {
    const [title, options] = await push(undefined);
    expect(title).toBe("Fleet");
    expect(options).toEqual({ body: "", icon: "/app-icons/icon-192.png", data: { url: ORIGIN } });
  });
});
