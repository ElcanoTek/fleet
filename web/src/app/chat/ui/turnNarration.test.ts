import { describe, expect, it } from "vitest";
import type { Message } from "./history";
import { joinTextSegments } from "./textSegments";
import {
  NARRATION_HOLD_CHARS,
  assistantDisplayContent,
  foldNarration,
  settleNarration,
} from "./turnNarration";

const msg = (over: Partial<Message> = {}): Message => ({
  id: 2,
  role: "assistant",
  content: "",
  state: "streaming",
  ...over,
});

describe("joinTextSegments", () => {
  it.each([
    ["", "Found it.", "Found it."],
    ["  \n", "Found it.", "Found it."],
    ["Let me check.", "", "Let me check."],
    // The reported bug: no space after the period.
    ["Let me check the deal.", "Found it.", "Let me check the deal.\n\nFound it."],
    ["Let me check.\n", "Found it.", "Let me check.\n\nFound it."],
    ["Let me check.", "\n\nFound it.", "Let me check.\n\nFound it."],
    ["Let me check.\n\n", "Found it.", "Let me check.\n\nFound it."],
  ])("joins %j and %j as paragraphs", (prev, next, want) => {
    expect(joinTextSegments(prev, next)).toBe(want);
  });
});

describe("foldNarration", () => {
  it("moves the step's text into the narration at a tool call", () => {
    const once = foldNarration(msg({ content: "Let me check the deal." }));
    expect(once).toMatchObject({ content: "", narration: "Let me check the deal." });
    const twice = foldNarration({ ...once, content: "Found it. Now the targeting." });
    expect(twice.narration).toBe("Let me check the deal.\n\nFound it. Now the targeting.");
    expect(twice.content).toBe("");
  });

  it("leaves a step that wrote nothing, and a final answer, alone", () => {
    const empty = msg({ content: " " });
    expect(foldNarration(empty)).toBe(empty);
    const final = msg({ content: "Done.", finalAnswer: true });
    expect(foldNarration(final)).toBe(final);
  });
});

describe("settleNarration", () => {
  it("keeps the narration apart once text.replace delivered the answer", () => {
    const m = msg({ content: "Done.", narration: "Let me check.", finalAnswer: true });
    expect(settleNarration(m)).toBe(m);
  });

  it("puts a stopped turn's narration back in front of what it streamed", () => {
    const m = settleNarration(msg({ content: "Now updat", narration: "Let me check." }));
    expect(m.content).toBe("Let me check.\n\nNow updat");
    expect(m.narration).toBeUndefined();
  });
});

describe("assistantDisplayContent", () => {
  const short = "Let me check the deal.";
  const long = "x".repeat(NARRATION_HOLD_CHARS);

  it("holds a short in-progress segment back in the default view", () => {
    expect(assistantDisplayContent(msg({ content: short }), false)).toBe("");
  });

  it("shows it with Show details on", () => {
    expect(assistantDisplayContent(msg({ content: short }), true)).toBe(short);
  });

  it("shows an answer once it outgrows narration", () => {
    expect(assistantDisplayContent(msg({ content: long }), false)).toBe(long);
  });

  it("always shows a final or finished answer, however short", () => {
    expect(assistantDisplayContent(msg({ content: "Done.", finalAnswer: true }), false)).toBe("Done.");
    expect(assistantDisplayContent(msg({ content: "Done.", state: "done" }), false)).toBe("Done.");
  });
});
