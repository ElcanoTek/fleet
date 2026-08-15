import { describe, expect, it } from "vitest";
import { liveSubagentActivity } from "./LogViewer";
import type { TaskStreamFrame } from "@/app/shared/lib/orchestratorApi";

// A scheduled run's live sub-agent card (#1043 follow-up). The child's raw
// events used to land in the parent's activity feed unattributed; they now
// arrive as subagent_progress frames and fold into the spawn entry's own card.

const frame = (f: Partial<TaskStreamFrame>): TaskStreamFrame =>
  ({ type: "subagent_progress", tool_call_id: "call-1", ...f }) as TaskStreamFrame;

describe("liveSubagentActivity", () => {
  it("seeds identity from the started frame", () => {
    const a = liveSubagentActivity(undefined, frame({
      phase: "started", child_session_id: "subagent-abc", role: "explore",
    }));
    expect(a).toEqual({
      childSessionId: "subagent-abc",
      role: "explore",
      steps: 0,
      current: "starting…",
    });
  });

  it("tracks the current tool and the step count", () => {
    let a = liveSubagentActivity(undefined, frame({ phase: "started", role: "worker" }));
    a = liveSubagentActivity(a, frame({ phase: "tool", tool: "bash", detail: "command=ls", step: 1 }));
    expect(a.current).toBe("bash · command=ls");
    expect(a.steps).toBe(1);

    a = liveSubagentActivity(a, frame({ phase: "tool_result", tool: "bash", step: 1 }));
    expect(a.current).toBe("bash returned");
    // A later frame without a step number must not reset the count.
    a = liveSubagentActivity(a, frame({ phase: "text", detail: "writing it up" }));
    expect(a.steps).toBe(1);
    expect(a.current).toBe("writing: writing it up");
  });

  it("clears the current action when the child finishes", () => {
    let a = liveSubagentActivity(undefined, frame({ phase: "tool", tool: "web_fetch", step: 3 }));
    a = liveSubagentActivity(a, frame({ phase: "finished", success: true, steps: 4 }));
    expect(a.current).toBeUndefined();
    expect(a.steps).toBe(4);
  });

  it("never mutates the previous activity", () => {
    const first = liveSubagentActivity(undefined, frame({ phase: "started", role: "explore" }));
    const snapshot = JSON.stringify(first);
    liveSubagentActivity(first, frame({ phase: "tool", tool: "bash", step: 1 }));
    expect(JSON.stringify(first)).toBe(snapshot);
  });
});
