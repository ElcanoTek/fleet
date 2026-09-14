import { describe, expect, it } from "vitest";
import { buildPromptWithRecipients, splitPromptRecipients } from "./taskEmailBlock";

const BASE = "Pull yesterday's delivery numbers and summarize them.";

describe("buildPromptWithRecipients", () => {
  it("leaves a prompt with no recipients exactly as the author typed it", () => {
    expect(buildPromptWithRecipients(BASE, [])).toBe(BASE);
  });

  it("appends the delivery instruction when there is someone to deliver to", () => {
    const built = buildPromptWithRecipients(BASE, ["ops@example.com"]);
    expect(built.startsWith(BASE)).toBe(true);
    expect(built).toContain("CRITICAL ACTION");
    expect(built).toContain("    - ops@example.com");
  });
});

describe("splitPromptRecipients", () => {
  // The regression this module exists for: before the round-trip, editing a
  // task opened with an empty recipient list, so re-selecting a library prompt
  // replaced the whole prompt — block and all — and the next run emailed its
  // report to nobody.
  it("recovers the recipients an edit used to drop", () => {
    const stored = buildPromptWithRecipients(BASE, ["a@example.com", "b@example.com"]);
    expect(splitPromptRecipients(stored)).toEqual({
      basePrompt: BASE,
      recipients: ["a@example.com", "b@example.com"],
    });
  });

  it("round-trips without growing the prompt on repeated edits", () => {
    let stored = buildPromptWithRecipients(BASE, ["a@example.com"]);
    for (let i = 0; i < 3; i++) {
      const { basePrompt, recipients } = splitPromptRecipients(stored);
      stored = buildPromptWithRecipients(basePrompt, recipients);
    }
    expect(stored).toBe(buildPromptWithRecipients(BASE, ["a@example.com"]));
  });

  it("survives the user replacing the prompt: recipients are held apart from it", () => {
    const stored = buildPromptWithRecipients(BASE, ["a@example.com"]);
    const { recipients } = splitPromptRecipients(stored);
    // What the library's onInsert does — replace the base text outright.
    expect(buildPromptWithRecipients("A completely different prompt.", recipients)).toContain(
      "    - a@example.com",
    );
  });

  it("returns a plain prompt untouched", () => {
    expect(splitPromptRecipients(BASE)).toEqual({ basePrompt: BASE, recipients: [] });
  });

  // The conservative half of the contract. In each of these the block is not
  // one this module would have written, so it stays in the textarea where its
  // author can see it, rather than being quietly eaten.
  it.each([
    [
      "a hand-written block that only resembles ours",
      `${BASE}\n\n---\nCRITICAL ACTION\nemail:\n  recipients:\n    - a@example.com\n---`,
    ],
    [
      "a block with an unparseable address",
      buildPromptWithRecipients(BASE, ["a@example.com"]).replace("a@example.com", "not-an-email"),
    ],
    [
      "a block with no recipients listed",
      buildPromptWithRecipients(BASE, ["a@example.com"]).replace("    - a@example.com\n", ""),
    ],
    [
      "a block the author kept writing after",
      `${buildPromptWithRecipients(BASE, ["a@example.com"])}\n\nAnd one more thing.`,
    ],
  ])("leaves %s in the prompt", (_label, stored) => {
    expect(splitPromptRecipients(stored)).toEqual({ basePrompt: stored, recipients: [] });
  });

  // Legacy data the OLD form actually wrote. Editing a task opened with an empty
  // recipient list while keeping the stored block in the textarea, so adding an
  // address during an edit saved a second block on top of the first. The agent
  // reads the prompt and honours both, so both sets really are receiving mail.
  const legacyDoubleBlock = (first: string[], second: string[]) =>
    // Exactly what the old buildFinalPrompt produced: base (which already ends
    // in a block) + a freshly appended one.
    buildPromptWithRecipients(buildPromptWithRecipients(BASE, first), second);

  it("recovers recipients from every block a double-saved task accumulated", () => {
    const stored = legacyDoubleBlock(["first@example.com"], ["second@example.com"]);
    expect(splitPromptRecipients(stored)).toEqual({
      basePrompt: BASE,
      recipients: ["first@example.com", "second@example.com"],
    });
  });

  it("leaves no block behind in the prompt the author sees", () => {
    const { basePrompt } = splitPromptRecipients(
      legacyDoubleBlock(["a@example.com"], ["b@example.com"]),
    );
    expect(basePrompt).not.toContain("CRITICAL ACTION");
  });

  it("collapses an address that appears in more than one block to a single chip", () => {
    // The common shape: the second save re-listed the original recipient
    // alongside the newly added one.
    const stored = legacyDoubleBlock(["a@example.com"], ["a@example.com", "b@example.com"]);
    expect(splitPromptRecipients(stored).recipients).toEqual([
      "a@example.com",
      "b@example.com",
    ]);
  });

  it("normalizes a multi-block task to one block on the next save", () => {
    const stored = legacyDoubleBlock(["a@example.com"], ["b@example.com"]);
    const { basePrompt, recipients } = splitPromptRecipients(stored);
    const resaved = buildPromptWithRecipients(basePrompt, recipients);
    expect(resaved.match(/CRITICAL ACTION/g)).toHaveLength(1);
    expect(splitPromptRecipients(resaved).recipients).toEqual(recipients);
  });

  it("removing a chip from a multi-block task really stops that mail", () => {
    // The sharp edge of recovering only the last block: the hidden earlier one
    // would keep mailing someone the form no longer shows.
    const stored = legacyDoubleBlock(["ghost@example.com"], ["b@example.com"]);
    const { basePrompt, recipients } = splitPromptRecipients(stored);
    const remaining = recipients.filter((e) => e !== "ghost@example.com");
    expect(buildPromptWithRecipients(basePrompt, remaining)).not.toContain("ghost@example.com");
  });

  it("takes the addresses verbatim rather than reformatting them", () => {
    const mixed = ["First.Last+tag@Example.com", "b@example.com"];
    const { recipients } = splitPromptRecipients(buildPromptWithRecipients(BASE, mixed));
    expect(recipients).toEqual(mixed);
  });
});
