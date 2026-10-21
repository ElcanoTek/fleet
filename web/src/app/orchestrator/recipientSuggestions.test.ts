import { describe, expect, it } from "vitest";
import type { RecipientUse } from "@/app/shared/lib/orchestratorApi";
import { rankRecipientSuggestions } from "./recipientSuggestions";

const use = (email: string, task_count: number, last_used = "2026-10-20T10:00:00Z"): RecipientUse => ({
  email,
  task_count,
  last_used,
});

const HISTORY = [
  use("mandy.kageff@weather.com", 3, "2026-10-21T06:00:00Z"),
  use("jeanne@elcanotek.com", 32),
  use("jane.blumin@weather.com", 3, "2026-10-19T06:00:00Z"),
  use("connor@elcanotek.com", 18),
  use("cs_am@weather.com", 12),
];

describe("rankRecipientSuggestions", () => {
  it("offers the most used addresses first when nothing is typed, minus the ones already added", () => {
    expect(rankRecipientSuggestions(HISTORY, "", ["connor@elcanotek.com"]).map((r) => r.email)).toEqual([
      "jeanne@elcanotek.com",
      "cs_am@weather.com",
      "mandy.kageff@weather.com",
      "jane.blumin@weather.com",
    ]);
  });

  it("matches what was typed, word starts before mid-word hits", () => {
    expect(rankRecipientSuggestions(HISTORY, "blu", []).map((r) => r.email)).toEqual(["jane.blumin@weather.com"]);
    // "weather" starts the domain of every weather.com address; usage orders them.
    expect(rankRecipientSuggestions(HISTORY, "Weather", []).map((r) => r.email)).toEqual([
      "cs_am@weather.com",
      "mandy.kageff@weather.com",
      "jane.blumin@weather.com",
    ]);
    // A prefix of the whole address beats a word start elsewhere.
    expect(rankRecipientSuggestions(HISTORY, "c", []).map((r) => r.email).slice(0, 2)).toEqual([
      "connor@elcanotek.com",
      "cs_am@weather.com",
    ]);
    expect(rankRecipientSuggestions(HISTORY, "nobody", [])).toEqual([]);
  });

  it("caps the list", () => {
    expect(rankRecipientSuggestions(HISTORY, "", [], 2)).toHaveLength(2);
  });
});
