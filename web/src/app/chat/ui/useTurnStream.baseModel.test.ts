import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream } from "./useTurnStream";
import { closedStream, createTurnStreamHarness } from "./turnStreamTestHarness";
import type { ConversationSummary } from "./chat-experience";

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

describe("useTurnStream base_model echo protection across model rollouts", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("sends base_model for passively hydrated conversations, adopts server rollout model from conversation SSE, and omits base_model after explicit user pick", async () => {
    const convId = "conv-rollout-1";
    const serverModelByConvRef = {
      current: new Map<string, string>([[convId, "anthropic/claude-opus-5.5"]]),
    };
    let latestSelectedModel = "anthropic/claude-opus-5.5";
    let sidebarConvs: ConversationSummary[] = [
      {
        id: convId,
        title: "Existing chat",
        persona: "default",
        model: "anthropic/claude-opus-5.5",
        pinned: false,
        updated_at: 1000,
      },
    ];
    const postedBodies: Array<Record<string, unknown>> = [];

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
      if (url === "/api/chat") {
        const parsed = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
        postedBodies.push(parsed);
        return new Response(
          closedStream([
            sse(1, "conversation", {
              id: convId,
              title: "Existing chat",
              persona: "default",
              model: "google/gemini-4-hydrogen",
            }),
            sse(2, "turn.completed", {}),
          ]),
          { status: 200, headers: { "Content-Type": "text/event-stream" } },
        );
      }
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    });

    const harness = createTurnStreamHarness({
      conversationId: convId,
      overrides: {
        selectedModel: latestSelectedModel,
        serverModelByConvRef,
        setSelectedModel: (next) => {
          latestSelectedModel = typeof next === "function" ? next(latestSelectedModel) : next;
        },
        setConversations: (updater) => {
          sidebarConvs = typeof updater === "function" ? updater(sidebarConvs) : updater;
        },
      },
    });

    const { result, rerender } = renderHook(
      (deps) => useTurnStream(deps),
      { initialProps: harness.deps },
    );

    // 1) Passively hydrated tab sends both model and base_model, then adopts
    //    google/gemini-4-hydrogen when the server responds with the rolled-out model.
    await act(async () => {
      await result.current.submitPrompt("hello from open tab");
    });

    expect(postedBodies).toHaveLength(1);
    expect(postedBodies[0].model).toBe("anthropic/claude-opus-5.5");
    expect(postedBodies[0].base_model).toBe("anthropic/claude-opus-5.5");
    expect(latestSelectedModel).toBe("google/gemini-4-hydrogen");
    expect(serverModelByConvRef.current.get(convId)).toBe("google/gemini-4-hydrogen");
    expect(sidebarConvs[0]?.model).toBe("google/gemini-4-hydrogen");

    // 2) User explicitly picks a model in the picker (clearing serverModelByConvRef):
    //    base_model is omitted so the server persists the override.
    serverModelByConvRef.current.delete(convId);
    latestSelectedModel = "anthropic/claude-opus-5.5";
    rerender({ ...harness.deps, selectedModel: latestSelectedModel });

    await act(async () => {
      await result.current.submitPrompt("explicitly use opus");
    });

    expect(postedBodies).toHaveLength(2);
    expect(postedBodies[1].model).toBe("anthropic/claude-opus-5.5");
    expect(postedBodies[1].base_model).toBeUndefined();
  });
});
