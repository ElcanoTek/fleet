import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PromptLibrary } from "./PromptLibrary";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    prompts: vi.fn(),
    createPrompt: vi.fn(),
    updatePrompt: vi.fn(),
    deletePrompt: vi.fn(),
  },
}));

const gitPrompt = {
  id: "git:daily.yaml",
  name: "Daily scan",
  description: "Check system health",
  content: "name: Daily scan\nsteps:\n  - inspect",
  source: "git" as const,
  visibility: "workspace" as const,
  read_only: true,
  owned_by_caller: false,
  path: "prompts/daily.yaml",
};

describe("PromptLibrary", () => {
  beforeEach(() => {
    vi.mocked(orchestratorApi.prompts).mockResolvedValue([gitPrompt]);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("loads a Git-backed prompt and inserts its exact content", async () => {
    const onInsert = vi.fn();
    render(<PromptLibrary currentText="" onInsert={onInsert} />);
    fireEvent.click(screen.getByRole("button", { name: "Open prompt library" }));
    expect(await screen.findAllByText("Daily scan")).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));
    expect(onInsert).toHaveBeenCalledWith(gitPrompt.content);
    expect(screen.queryByRole("dialog", { name: "Prompt library" })).not.toBeInTheDocument();
  });

  it("starts a private workspace prompt from the current draft", async () => {
    vi.mocked(orchestratorApi.createPrompt).mockResolvedValue({
      ...gitPrompt,
      id: "custom-id",
      source: "workspace",
      visibility: "private",
      read_only: false,
      owned_by_caller: true,
    });
    render(<PromptLibrary currentText="my reusable draft" onInsert={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Open prompt library" }));
    await screen.findAllByText("Daily scan");
    fireEvent.click(screen.getByRole("button", { name: "New prompt" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "My prompt" } });
    expect(screen.getByLabelText("Prompt")).toHaveValue("my reusable draft");
    fireEvent.click(screen.getByRole("button", { name: "Save prompt" }));
    await waitFor(() => expect(orchestratorApi.createPrompt).toHaveBeenCalledWith({
      name: "My prompt",
      description: "",
      content: "my reusable draft",
      visibility: "private",
    }));
  });
});

it("portals the dialog to <body> so transformed ancestors can't trap position:fixed", async () => {
  vi.mocked(orchestratorApi.prompts).mockResolvedValue([]);
  // The chat composer wraps this component in transform-animated containers;
  // position:fixed resolves against the nearest transformed ancestor, which
  // used to shove the dialog half off-screen.
  render(
    <div style={{ transform: "translateZ(0)" }}>
      <PromptLibrary currentText="" onInsert={() => {}} compact />
    </div>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Open prompt library" }));
  const dialog = await screen.findByRole("dialog", { name: "Prompt library" });
  expect(dialog.parentElement).toBe(document.body);
});
