import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getGatedJson } from "./getGated";

const assign = vi.fn();

beforeEach(() => {
  vi.stubGlobal("location", { ...window.location, assign });
});

afterEach(() => {
  assign.mockReset();
  vi.unstubAllGlobals();
});

function reply(body: string, status: number) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(body, { status })));
}

const opts = { label: "Users", statusText: { 403: "You are not an admin.", 501: "Not here." } };

describe("getGatedJson", () => {
  it("answers the parsed body on a 2xx, read uncached", async () => {
    reply(JSON.stringify({ users: [1] }), 200);
    await expect(getGatedJson<{ users: number[] }>("/api/admin/users", opts)).resolves.toEqual({ users: [1] });
    expect(fetch).toHaveBeenCalledWith("/api/admin/users", { cache: "no-store" });
    expect(assign).not.toHaveBeenCalled();
  });

  it("sends a 401 to /login and answers null", async () => {
    reply("", 401);
    await expect(getGatedJson("/api/admin/users", opts)).resolves.toBeNull();
    expect(assign).toHaveBeenCalledWith("/login");
  });

  it("throws the caller's sentence for a named status", async () => {
    reply("forbidden", 403);
    await expect(getGatedJson("/x", opts)).rejects.toThrow("You are not an admin.");
    reply("", 501);
    await expect(getGatedJson("/x", opts)).rejects.toThrow("Not here.");
  });

  it("throws the server's reason, or the labelled status, for anything else", async () => {
    reply("db locked", 500);
    await expect(getGatedJson("/x", opts)).rejects.toThrow("db locked");
    reply("", 502);
    await expect(getGatedJson("/x", opts)).rejects.toThrow("Users request failed (502)");
    reply("", 403);
    await expect(getGatedJson("/x", { label: "Files" })).rejects.toThrow("Files request failed (403)");
  });
});
