import { describe, expect, it } from "vitest";
import { RESPONSE_ERROR_MAX_CHARS, responseErrorText } from "./responseError";

function res(body: string, status = 500): Response {
  return new Response(body, { status });
}

describe("responseErrorText", () => {
  it("returns the server's trimmed reason", async () => {
    expect(await responseErrorText(res("  name already taken\n", 409), "Save failed")).toBe(
      "name already taken",
    );
  });

  it("falls back with the status on an empty body", async () => {
    expect(await responseErrorText(res("", 502), "Save failed")).toBe("Save failed (502)");
  });

  it("treats a whitespace-only body as empty", async () => {
    expect(await responseErrorText(res(" \n\t ", 500), "Delete failed")).toBe("Delete failed (500)");
  });

  it("caps an over-long body and marks the cut", async () => {
    const long = "x".repeat(RESPONSE_ERROR_MAX_CHARS + 50);
    const out = await responseErrorText(res(long), "Save failed");
    expect(out).toBe(`${"x".repeat(RESPONSE_ERROR_MAX_CHARS)}…`);
  });

  it("keeps a body of exactly the cap uncut", async () => {
    const exact = "y".repeat(RESPONSE_ERROR_MAX_CHARS);
    expect(await responseErrorText(res(exact), "Save failed")).toBe(exact);
  });

  it("falls back on a proxy's HTML error page", async () => {
    const page = "<!DOCTYPE html><html><body><h1>502 Bad Gateway</h1></body></html>";
    expect(await responseErrorText(res(page, 502), "Save failed")).toBe("Save failed (502)");
    const fragment = new Response("<h1>Bad Gateway</h1>", {
      status: 502,
      headers: { "Content-Type": "text/html; charset=utf-8" },
    });
    expect(await responseErrorText(fragment, "Save failed")).toBe("Save failed (502)");
  });

  it("keeps a plain-text reason that merely mentions a tag", async () => {
    expect(await responseErrorText(res("name must not contain <script>", 400), "Save failed")).toBe(
      "name must not contain <script>",
    );
  });

  it("never throws when reading the body fails", async () => {
    const broken = {
      status: 503,
      text: () => Promise.reject(new Error("stream reset")),
    } as unknown as Response;
    expect(await responseErrorText(broken, "Unable to load")).toBe("Unable to load (503)");
  });

  it("never throws when the body was already consumed", async () => {
    const r = res("first read", 400);
    await r.text();
    expect(await responseErrorText(r, "Update failed")).toBe("Update failed (400)");
  });
});
