import { describe, expect, it } from "vitest";
import { jsonResponse, stubFetch, textResponse } from "./fetch";

describe("test fetch helpers", () => {
  it("builds real Responses whose ok follows the status", async () => {
    const ok = jsonResponse({ a: 1 });
    expect(ok.ok).toBe(true);
    expect(ok.headers.get("Content-Type")).toBe("application/json");
    expect(await ok.json()).toEqual({ a: 1 });
    const refused = textResponse("over the cap", 409);
    expect(refused.ok).toBe(false);
    expect(await refused.text()).toBe("over the cap");
    expect(jsonResponse({}, 204).body).toBeNull();
  });

  it("routes by method + URL, then URL, then path; unmatched is 404", async () => {
    const fetchMock = stubFetch({
      "POST /api/x": { posted: true },
      "/api/x?full=1": { full: true },
      "/api/x": (url) => ({ got: url }),
      "/api/r": jsonResponse({ again: true }),
    });
    expect(await (await fetch("/api/x", { method: "POST" })).json()).toEqual({ posted: true });
    expect(await (await fetch("/api/x?full=1")).json()).toEqual({ full: true });
    expect(await (await fetch("/api/x?q=2")).json()).toEqual({ got: "/api/x?q=2" });
    // A Response route is cloned, so it can be read on every call.
    expect(await (await fetch("/api/r")).json()).toEqual({ again: true });
    expect(await (await fetch("/api/r")).json()).toEqual({ again: true });
    expect((await fetch("/nope")).status).toBe(404);
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });

  it("passes every call to an implementation function", async () => {
    stubFetch((url, init) => textResponse(`${init?.method ?? "GET"} ${url}`));
    expect(await (await fetch("/api/y", { method: "DELETE" })).text()).toBe("DELETE /api/y");
  });
});
