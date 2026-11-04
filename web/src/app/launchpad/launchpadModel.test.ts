import { describe, expect, it } from "vitest";
import {
  CADENCE_PRESETS,
  draftPrompt,
  draftTitle,
  initialSelection,
  matchingConnectors,
  optionalSelection,
  PATROL_GOALS,
  type LaunchpadConnector,
} from "./launchpadModel";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { parsePatrolCron } from "@/app/shared/lib/patrolSchedule";

// The Patrol Launchpad's pure half (docs/PATROL-LAUNCHPAD.md): goal↔connector
// matching, the connector set a patrol chat persists, and the template draft
// the wizard launches when the assistant is not asked to write one.

const gmail: LaunchpadConnector = { name: "gmail", display_name: "Gmail", description: "Read and search mail", enabled: true };
const deals: LaunchpadConnector = { name: "dealdesk", display_name: "Deal Desk", description: "PMP deal management", category: "deal-management" };
const setup: LaunchpadConnector = { name: "trade", display_name: "Trade Desk", description: "DSP campaigns", status: "needs_setup" };
const always: LaunchpadConnector = { name: "files", display_name: "Files", always_on: true, enabled: true };

const goal = (id: string) => PATROL_GOALS.find((g) => g.id === id)!;

describe("launchpadModel", () => {
  it("matches a goal to the connectors that can serve it, never to one that needs setup", () => {
    expect(matchingConnectors(goal("inbox"), [gmail, deals, setup]).map((c) => c.name)).toEqual(["gmail"]);
    expect(matchingConnectors(goal("deals"), [gmail, deals, setup]).map((c) => c.name)).toEqual(["dealdesk"]);
    expect(matchingConnectors(goal("anomalies"), [gmail])).toEqual([]);
  });

  it("pre-ticks what a new chat would have on, and persists only optional, usable picks", () => {
    expect(initialSelection([gmail, deals, setup, always])).toEqual(["gmail", "files"]);
    expect(optionalSelection([gmail, deals, setup, always], ["gmail", "dealdesk", "trade", "files"])).toEqual([
      "gmail",
      "dealdesk",
    ]);
  });

  it("names the patrol from its goals", () => {
    expect(draftTitle([])).toBe("My patrol");
    expect(draftTitle(["inbox"])).toBe("Inbox patrol");
    expect(draftTitle(["spend", "deals"])).toBe("Deals & spend patrol");
    expect(draftTitle(["deals", "spend", "inbox"])).toBe("Deals, spend & more patrol");
  });

  it("writes a template draft that names the tools per check and says what is missing", () => {
    const text = draftPrompt({
      goals: ["deals", "inbox", "anomalies"],
      notes: "Watch the Acme deals closely.",
      connectors: [gmail, deals, setup],
      selected: ["gmail", "dealdesk", "trade"],
      mode: "suggest",
      cadenceLabel: "Weekday mornings, UTC",
    });
    expect(text).toContain("**Mission**");
    expect(text).toMatch(/1\. Check every active deal.*\(use Deal Desk\)/);
    expect(text).toMatch(/2\. Scan new email.*\(use Gmail\)/);
    expect(text).toMatch(/3\. .*tell me once that it needs a database/);
    expect(text).toContain("4. Watch the Acme deals closely.");
    expect(text).toContain("stage each one for my approval");
    expect(text).toContain("Tools switched on for this patrol: Gmail and Deal Desk.");
    expect(text).not.toContain("Trade Desk");
    expect(text).toContain("Schedule: Weekday mornings, UTC.");
  });

  it("states each autonomy mode's boundary", () => {
    const base = { goals: ["inbox"], notes: "", connectors: [gmail], selected: ["gmail"], cadenceLabel: "" };
    expect(draftPrompt({ ...base, mode: "report" })).toContain("Never change anything");
    expect(draftPrompt({ ...base, mode: "act" })).toContain("list every action you took");
  });

  it("offers only cadences that read in words and sit well above the server's five-minute floor", () => {
    for (const p of CADENCE_PRESETS) {
      expect(describeCronExpression(p.cron), p.cron).not.toBe("");
      expect(parsePatrolCron(p.cron).frequency, p.cron).not.toBe("minutes");
    }
  });
});
