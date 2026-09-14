import { describe, expect, it } from "vitest";
import { NextRequest } from "next/server";

import { passThroughQuery } from "./proxy";

const ORIGIN = "https://chat.example.com";

function request(query: string): NextRequest {
  return new NextRequest(`${ORIGIN}/api/orchestrator/tasks?${query}`);
}

// passThroughQuery is the allow-list between the browser and the orchestrator.
// It read only the FIRST value of each param, which is correct for every
// single-valued filter and wrong for `tag`: ?tag=a&tag=b means "carrying BOTH"
// (the server ANDs them), so dropping b WIDENED the result instead of
// narrowing it — the one direction a filter must never fail in.
describe("passThroughQuery", () => {
  it("forwards every value of a repeated param", () => {
    const qs = passThroughQuery(request("tag=ops&tag=urgent"), ["tag"]);
    expect(new URLSearchParams(qs.slice(1)).getAll("tag")).toEqual(["ops", "urgent"]);
  });

  it("passes a single-valued param through unchanged", () => {
    expect(passThroughQuery(request("status=running"), ["status"])).toBe("?status=running");
  });

  it("drops params outside the allow-list", () => {
    expect(passThroughQuery(request("status=running&secret=x"), ["status"])).toBe(
      "?status=running",
    );
    expect(passThroughQuery(request("tag=ops"), ["status"])).toBe("");
  });

  it("drops empty values rather than forwarding a blank filter", () => {
    expect(passThroughQuery(request("status=&q=hello"), ["status", "q"])).toBe("?q=hello");
    // An empty value among repeated ones drops only itself.
    const qs = passThroughQuery(request("tag=ops&tag=&tag=urgent"), ["tag"]);
    expect(new URLSearchParams(qs.slice(1)).getAll("tag")).toEqual(["ops", "urgent"]);
  });

  it("returns an empty string, not a bare '?', when nothing passes", () => {
    expect(passThroughQuery(request("nope=1"), ["status"])).toBe("");
  });

  it("emits params in allow-list order, not the caller's", () => {
    expect(passThroughQuery(request("q=hi&status=running"), ["status", "q"])).toBe(
      "?status=running&q=hi",
    );
  });

  it("keeps a comma-separated value intact for completed_status", () => {
    // The Failed Today card sends two statuses in one value; splitting or
    // truncating it here would silently halve the filter.
    const qs = passThroughQuery(
      request("completed_status=error%2Cdead_lettered"),
      ["completed_status"],
    );
    expect(new URLSearchParams(qs.slice(1)).get("completed_status")).toBe("error,dead_lettered");
  });
});
