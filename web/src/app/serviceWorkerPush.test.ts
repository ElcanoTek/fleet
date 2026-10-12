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
  const showNotification = vi.fn(async () => {});
  const scope = {
    location: new URL(`${ORIGIN}/sw.js`),
    clients: {},
    skipWaiting: vi.fn(),
    registration: { showNotification },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
  };
  // `clients` is also a global in a real worker scope.
  new Function("self", "clients", SOURCE)(scope, scope.clients);
  return { listeners, showNotification };
}

async function push(data: { json: () => unknown } | undefined) {
  const worker = loadWorker();
  let shown: Promise<unknown> = Promise.resolve();
  worker.listeners.push({
    data,
    waitUntil: (p: Promise<unknown>) => {
      shown = p;
    },
  });
  await shown;
  expect(worker.showNotification).toHaveBeenCalledTimes(1);
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
    expect(Object.keys(options).sort()).toEqual(["body", "data", "icon"]);
    expect(options.data).toEqual({ url: `${ORIGIN}/chat?c=1` });
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
    const [title] = await push(undefined);
    expect(title).toBe("Fleet");
  });
});
