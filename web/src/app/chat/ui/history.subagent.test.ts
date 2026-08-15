import { describe, expect, it } from "vitest";
import {
  applySubagentProgress,
  liveSubagentLabel,
  SUBAGENT_TRAIL_LIMIT,
  type SubagentProgressEventPayload,
  type ToolCall,
} from "./history";

// Live sub-agent activity (#1043 follow-up). The reducer is what turns the
// server's subagent.progress events into the card an operator watches while a
// delegation runs — before it, a spawn was a spinner over the task text for the
// child's whole lifetime.

const ev = (p: SubagentProgressEventPayload): SubagentProgressEventPayload => p;

describe("applySubagentProgress", () => {
  it("seeds the card from the started event", () => {
    const a = applySubagentProgress(undefined, ev({
      phase: "started",
      child_session_id: "subagent-abc",
      role: "explore",
      task: "summarize the inbox",
      workdir: "/ws/subagents/subagent-abc",
      model: "inherited(parent)",
    }));
    expect(a.childId).toBe("subagent-abc");
    expect(a.role).toBe("explore");
    expect(a.phase).toBe("started");
    expect(a.workdir).toBe("/ws/subagents/subagent-abc");
    expect(a.current).toBe("starting…");
    expect(a.steps).toBe(0);
  });

  it("records tool steps with their argument summary and dedupes tools used", () => {
    let a = applySubagentProgress(undefined, ev({ phase: "started", role: "explore" }));
    a = applySubagentProgress(a, ev({
      phase: "tool", tool: "outlook_email_search", step: 1, detail: "query=is:unread",
    }));
    a = applySubagentProgress(a, ev({ phase: "tool_result", tool: "outlook_email_search", step: 1 }));
    a = applySubagentProgress(a, ev({ phase: "tool", tool: "outlook_email_search", step: 2 }));

    expect(a.steps).toBe(2);
    expect(a.toolsUsed).toEqual(["outlook_email_search"]);
    expect(a.trail).toHaveLength(3);
    expect(a.trail[0]).toMatchObject({ phase: "tool", tool: "outlook_email_search" });
  });

  it("keeps text previews as the current line, not trail entries", () => {
    let a = applySubagentProgress(undefined, ev({ phase: "started", role: "worker" }));
    a = applySubagentProgress(a, ev({ phase: "text", detail: "The three emails are…" }));
    expect(a.current).toBe("writing: The three emails are…");
    expect(a.trail).toHaveLength(0);

    a = applySubagentProgress(a, ev({ phase: "thinking", detail: "checking dates" }));
    expect(a.current).toBe("thinking: checking dates");
  });

  it("bounds the trail so a long child cannot grow it without limit", () => {
    let a = applySubagentProgress(undefined, ev({ phase: "started" }));
    for (let i = 1; i <= SUBAGENT_TRAIL_LIMIT + 4; i++) {
      a = applySubagentProgress(a, ev({ phase: "tool", tool: `tool_${i}`, step: i }));
    }
    expect(a.trail).toHaveLength(SUBAGENT_TRAIL_LIMIT);
    expect(a.trail[a.trail.length - 1].tool).toBe(`tool_${SUBAGENT_TRAIL_LIMIT + 4}`);
  });

  it("settles on the finished event with spend, steps and status", () => {
    let a = applySubagentProgress(undefined, ev({ phase: "started", role: "explore" }));
    a = applySubagentProgress(a, ev({ phase: "tool", tool: "web_fetch", step: 1 }));
    a = applySubagentProgress(a, ev({
      phase: "finished",
      success: true,
      cost_usd: 0.0123,
      tokens: 4210,
      steps: 3,
      tools_used: ["web_fetch", "view_file"],
      duration_ms: 8400,
    }));
    expect(a.phase).toBe("finished");
    expect(a.success).toBe(true);
    expect(a.costUsd).toBeCloseTo(0.0123);
    expect(a.tokens).toBe(4210);
    expect(a.steps).toBe(3);
    expect(a.toolsUsed).toEqual(["web_fetch", "view_file"]);
    expect(a.durationMs).toBe(8400);
    // Nothing is "in flight" any more.
    expect(a.current).toBeUndefined();
  });

  it("carries the failure note of an unsuccessful child", () => {
    const a = applySubagentProgress(undefined, ev({
      phase: "finished", success: false, note: "no final answer", steps: 0,
    }));
    expect(a.success).toBe(false);
    expect(a.note).toBe("no final answer");
  });

  it("never mutates the previous activity", () => {
    const first = applySubagentProgress(undefined, ev({ phase: "started", role: "explore" }));
    const snapshot = JSON.stringify(first);
    applySubagentProgress(first, ev({ phase: "tool", tool: "bash", step: 1 }));
    expect(JSON.stringify(first)).toBe(snapshot);
  });
});

describe("liveSubagentLabel", () => {
  const call = (subagent?: ToolCall["subagent"], name = "spawn_subagent"): ToolCall => ({
    id: "call-1",
    name,
    input: "{}",
    state: "pending",
    subagent,
  });

  it("describes what the child is doing right now", () => {
    const label = liveSubagentLabel(
      call({
        childId: "subagent-abc",
        role: "explore",
        phase: "tool",
        steps: 2,
        toolsUsed: ["outlook_email_search"],
        trail: [],
        current: "outlook_email_search · query=is:unread",
      }),
    );
    expect(label).toBe("Sub-agent (explore) · step 2 · outlook_email_search · query=is:unread");
  });

  it("falls back to the role while the child has taken no steps", () => {
    expect(
      liveSubagentLabel(
        call({ childId: "c", role: "worker", phase: "started", steps: 0, toolsUsed: [], trail: [] }),
      ),
    ).toBe("Sub-agent (worker)");
  });

  it("returns null once finished, and for non-delegation calls", () => {
    expect(
      liveSubagentLabel(
        call({ childId: "c", role: "explore", phase: "finished", steps: 3, toolsUsed: [], trail: [] }),
      ),
    ).toBeNull();
    expect(liveSubagentLabel(call(undefined))).toBeNull();
    expect(
      liveSubagentLabel(
        call({ childId: "c", role: "explore", phase: "tool", steps: 1, toolsUsed: [], trail: [] }, "bash"),
      ),
    ).toBeNull();
  });
});
