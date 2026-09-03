import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SavePromptDialog } from "./SavePromptDialog";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    createPrompt: vi.fn(),
  },
}));

// The "Save to prompt library…" review dialog: it POSTs suggest-prompt to
// distill the conversation into a draft, lets the user edit it, and persists
// only on Save via the orchestrator's createPrompt.

const draft = {
  name: "Failed-task report",
  description: "Summarize failed scheduled tasks",
  content: "Summarize the scheduled tasks that failed in the last 24 hours.",
};

describe("SavePromptDialog", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify(draft), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("distills on open, prefills the form, and saves the edited draft as private", async () => {
    vi.mocked(orchestratorApi.createPrompt).mockResolvedValue({
      id: "p1",
    } as never);
    render(
      <SavePromptDialog
        conversationId="conv-1"
        conversationTitle="failed tasks"
        onClose={vi.fn()}
      />,
    );
    expect(screen.getByTestId("save-prompt-loading")).toBeInTheDocument();
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/conversations/conv-1/suggest-prompt",
      expect.objectContaining({ method: "POST" }),
    );

    const name = await screen.findByDisplayValue("Failed-task report");
    fireEvent.change(name, { target: { value: "Daily failure digest" } });
    fireEvent.click(screen.getByRole("button", { name: "Save prompt" }));

    await waitFor(() =>
      expect(orchestratorApi.createPrompt).toHaveBeenCalledWith({
        name: "Daily failure digest",
        description: draft.description,
        content: draft.content,
        visibility: "private",
      }),
    );
    expect(screen.getByTestId("save-prompt-saved")).toBeInTheDocument();
  });

  it("saves as workspace-shared when the share checkbox is ticked", async () => {
    vi.mocked(orchestratorApi.createPrompt).mockResolvedValue({
      id: "p2",
    } as never);
    render(
      <SavePromptDialog
        conversationId="conv-2"
        conversationTitle="t"
        onClose={vi.fn()}
      />,
    );
    await screen.findByDisplayValue(draft.name);
    fireEvent.click(
      screen.getByRole("checkbox", { name: "Share with this workspace" }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save prompt" }));
    await waitFor(() =>
      expect(orchestratorApi.createPrompt).toHaveBeenCalledWith(
        expect.objectContaining({ visibility: "workspace" }),
      ),
    );
  });

  it("offers a retry when distillation fails, without an empty form", async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(new Response("model unavailable", { status: 502 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify(draft), { status: 200 }),
      );
    render(
      <SavePromptDialog
        conversationId="conv-3"
        conversationTitle="t"
        onClose={vi.fn()}
      />,
    );
    const retry = await screen.findByRole("button", { name: "Try again" });
    expect(screen.queryByRole("button", { name: "Save prompt" })).toBeNull();
    fireEvent.click(retry);
    expect(await screen.findByDisplayValue(draft.name)).toBeInTheDocument();
  });

  it("does not persist anything when cancelled", async () => {
    const onClose = vi.fn();
    render(
      <SavePromptDialog
        conversationId="conv-4"
        conversationTitle="t"
        onClose={onClose}
      />,
    );
    await screen.findByDisplayValue(draft.name);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
    expect(orchestratorApi.createPrompt).not.toHaveBeenCalled();
  });

  it("cuts the distillation at the reply the user saved from", async () => {
    // "Save as prompt" under a message passes that reply's persisted id, so a
    // later tangent in the same chat can't leak into the saved recipe.
    render(
      <SavePromptDialog
        conversationId="conv-1"
        conversationTitle="failed tasks"
        upToMessageId={4242}
        onClose={vi.fn()}
      />,
    );
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/conversations/conv-1/suggest-prompt",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ up_to_message_id: 4242 }),
      }),
    );
    await waitFor(() =>
      expect(screen.getByDisplayValue(draft.name)).toBeInTheDocument(),
    );
    expect(
      screen.getByText(/up to the reply you saved from/i),
    ).toBeInTheDocument();
  });

});
