import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { UserTurn } from "./ChatTranscript";
import { QueuedInputs, queuedInputLabel } from "./QueuedInputs";
import { historyToMessages, type Approval, type HistoryEntry, type Message } from "./history";
import { showsEmptyReplyNotice } from "./transcriptRows";
import {
  approvalResumeFollowDelaysMs,
  useTurnStream,
  type QueuedInput,
} from "./useTurnStream";
import { closedStream, createTurnStreamHarness } from "./turnStreamTestHarness";

// Resume after approval (docs/RESUME-AFTER-APPROVAL.md): once an opted-in
// card is settled, fleet starts ONE turn on its own. The web has to (1) go
// and find that turn, since nothing it posted started it, (2) render its
// input as fleet's notice — never as words the user typed — live and after a
// reload, and (3) show the notes fleet writes when it does not start one.

const CONV = "conv-1";
const NOTICE = "[Approval resolved] mcp_deals_update_deal approval_id=ap1 outcome=approved. The result is in the conversation; continue the task.";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("historyToMessages", () => {
  it("marks fleet's resume input and hangs notices on the assistant row they follow", () => {
    const entries: HistoryEntry[] = [
      { role: "user", type: "text", content: { text: "update the deal" } },
      { role: "assistant", type: "text", content: { text: "Staged; I'll verify after approval." } },
      { role: "system", type: "notice", content: { kind: "approval_resume_skipped", text: "Automatic continue skipped." } },
      { role: "user", type: "text", content: { text: NOTICE, kind: "approval_resume" } },
      { role: "assistant", type: "text", content: { text: "Verified." } },
    ];
    const msgs = historyToMessages(entries);
    expect(msgs.map((m) => [m.role, m.kind])).toEqual([
      ["user", undefined],
      ["assistant", undefined],
      ["user", "approval_resume"],
      ["assistant", undefined],
    ]);
    expect(msgs[1].notices).toEqual(["Automatic continue skipped."]);
    // A row carrying only a note is not "finished without a written reply".
    expect(showsEmptyReplyNotice({ id: 9, role: "assistant", content: "", state: "done", notices: ["x"] })).toBe(false);
  });
});

describe("UserTurn", () => {
  it("renders fleet's resume input as a notice, not as the user's bubble, with no Edit", () => {
    const message: Message = { id: 1, role: "user", content: NOTICE, kind: "approval_resume", state: "done" };
    render(<UserTurn message={message} isLastUser isStreaming={false} editRequestSignal={0} onResend={() => {}} />);
    expect(screen.queryByTestId("user-message-bubble")).not.toBeInTheDocument();
    expect(screen.getByTestId("auto-continue-notice")).toHaveTextContent(/not typed by you/i);
    expect(screen.queryByRole("button", { name: /edit/i })).not.toBeInTheDocument();
  });
});

describe("QueuedInputs", () => {
  it("labels a waiting resume row as auto-continue and says what it is", () => {
    const row: QueuedInput = {
      id: "r1", client_input_id: "approval-resume:ap1", mode: "resume", state: "queued",
      position: 1, message_preview: NOTICE, has_attachments: false,
    };
    expect(queuedInputLabel(row)).toBe("auto-continue");
    render(<QueuedInputs items={[row]} onRemove={() => {}} onSendNow={() => {}} />);
    expect(screen.getByText(/continue after the approval/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove from queue" })).toBeInTheDocument();
  });
});

describe("ApprovalCard", () => {
  it("asks the transcript to follow when the server answers resume", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", result_text: "ok", is_err: false, resume: true }), { status: 200 }),
    ));
    const onResumeExpected = vi.fn();
    const approval: Approval = {
      id: "ap1", tool: "mcp_pages_deploy_page", status: "pending",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3" }] },
    };
    render(<ApprovalCard approval={approval} conversationId="c" onResolved={vi.fn()} onResumeExpected={onResumeExpected} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await waitFor(() => expect(onResumeExpected).toHaveBeenCalledWith(false));
  });

  it("follows for longer while the approved call is still executing", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", executing: true, resume: true }), { status: 200 }),
    ));
    const onResumeExpected = vi.fn();
    const approval: Approval = {
      id: "ap1", tool: "mcp_pages_deploy_page", status: "pending",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3" }] },
    };
    render(<ApprovalCard approval={approval} conversationId="c" onResolved={vi.fn()} onResumeExpected={onResumeExpected} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await waitFor(() => expect(onResumeExpected).toHaveBeenCalledWith(true));
  });

  it("does not follow when the server says nothing about a resume", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "rejected" }), { status: 200 }),
    ));
    const onResumeExpected = vi.fn();
    const onResolved = vi.fn();
    const approval: Approval = {
      id: "ap1", tool: "mcp_pages_deploy_page", status: "pending",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3" }] },
    };
    render(<ApprovalCard approval={approval} conversationId="c" onResolved={onResolved} onResumeExpected={onResumeExpected} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResumeExpected).not.toHaveBeenCalled();
  });
});

describe("followApprovalResume", () => {
  const sse = (id: number, event: string, data: unknown) =>
    `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
  const answered = (): Message[] => [
    { id: 1, role: "user", content: "update the deal", state: "done" },
    { id: 2, role: "assistant", content: "Staged; I'll verify after approval.", state: "done" },
  ];
  const persisted: HistoryEntry[] = [
    { role: "user", type: "text", content: { text: "update the deal" } },
    { role: "assistant", type: "text", content: { text: "Staged; I'll verify after approval." } },
  ];

  const stub = (inflight: Array<{ inflight: boolean; turn_id?: string }>, stream?: () => ReadableStream<Uint8Array>) => {
    let probes = 0;
    let attaches = 0;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
      if (url.includes("/inflight")) {
        const info = inflight[Math.min(probes, inflight.length - 1)];
        probes += 1;
        return json(info);
      }
      if (url.includes("/stream") && stream) {
        attaches += 1;
        return new Response(stream(), { status: 200, headers: { "content-type": "text/event-stream" } });
      }
      if (url.includes("/queue")) return json({ items: [] });
      if (url.includes("/api/conversations/")) return json({ history: persisted });
      return new Response("{}", { status: 404 });
    }));
    return { probes: () => probes, attaches: () => attaches };
  };

  it("attaches to the turn fleet started and renders its input as a notice", async () => {
    vi.useFakeTimers();
    const base = createTurnStreamHarness({ conversationId: CONV, initial: answered(), persisted });
    // Nothing yet on the first look (the server is still debouncing), then the turn.
    stub([{ inflight: false }, { inflight: true, turn_id: "t2" }], () =>
      closedStream([
        sse(1, "turn.started", { turn_id: "t2", input_id: "r1", queued: true, input_kind: "approval_resume" }),
        sse(2, "user.message", { text: NOTICE, kind: "approval_resume" }),
        sse(3, "turn.started", { persona: "generic", input_kind: "approval_resume" }),
        sse(4, "text.delta", { text: "Verified: the change is in place." }),
        sse(5, "turn.completed", { cost_usd: 0, duration_ms: 1 }),
      ]),
    );
    const { result } = renderHook(() => useTurnStream(base.deps));
    let done: Promise<void> = Promise.resolve();
    act(() => {
      done = result.current.followApprovalResume(CONV, 30_000);
    });
    await vi.advanceTimersByTimeAsync(approvalResumeFollowDelaysMs[0] + approvalResumeFollowDelaysMs[1] + 100);
    await done;
    const msgs = base.store.get(CONV) ?? [];
    expect(msgs.map((m) => [m.role, m.kind])).toEqual([
      ["user", undefined],
      ["assistant", undefined],
      ["user", "approval_resume"],
      ["assistant", undefined],
    ]);
    expect(msgs[3].content).toBe("Verified: the change is in place.");
    expect(base.loadConversationCalls).toEqual([]);
  });

  it("adopts the persisted transcript when no turn showed up within the wait", async () => {
    vi.useFakeTimers();
    const base = createTurnStreamHarness({ conversationId: CONV, initial: answered(), persisted });
    const calls = stub([{ inflight: false }]);
    const { result } = renderHook(() => useTurnStream(base.deps));
    let done: Promise<void> = Promise.resolve();
    act(() => {
      done = result.current.followApprovalResume(CONV, 6_000);
    });
    await vi.advanceTimersByTimeAsync(10_000);
    await done;
    expect(calls.attaches()).toBe(0);
    expect(calls.probes()).toBeGreaterThan(0);
    // Bounded: it stopped looking and read the canonical transcript once.
    expect(base.loadConversationCalls).toEqual([CONV]);
  });
});
