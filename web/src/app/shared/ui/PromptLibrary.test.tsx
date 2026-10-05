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
    // The entry's display name rides along so a caller can label what it just
    // inserted (the task form seeds an empty Title from it).
    expect(onInsert).toHaveBeenCalledWith(gitPrompt.content, gitPrompt.name);
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

// A Git prompt that declares a form (docs/PROMPT-LIBRARY.md, "Form prompts"),
// shaped as GET /prompts sends it: the raw YAML in `content`, plus the parsed
// `fields` and `prompt_template`.
const formPrompt = {
  ...gitPrompt,
  id: "git:new-campaign.yaml",
  name: "New campaign page",
  description: "A campaign dashboard from the partner's template.",
  content: "name: New campaign page\nfields:\n  - key: partner\n# …the raw YAML file…\n",
  path: "prompts/new-campaign.yaml",
  fields: [
    {
      key: "partner",
      label: "Partner",
      type: "select" as const,
      required: true,
      default: "TWC",
      options: ["TWC", "RainBarrel", "Other"],
    },
    { key: "campaign", label: "Campaign name", type: "text" as const, required: true, placeholder: "Go Raw CTV" },
    { key: "kpis", label: "Channel(s) and KPI target", type: "textarea" as const, required: true },
    { key: "deals", label: "Deals", type: "textarea" as const, advanced: true },
    { key: "shareable", label: "Client-shareable", type: "toggle" as const, advanced: true, default: false },
  ],
  prompt_template:
    "Create a new Pages dashboard (route A).\nPartner: {partner}\nCampaign: {campaign}\n" +
    "Channels and KPI targets: {kpis}\nDeals: {deals}\nClient-shareable: {shareable}",
};

describe("PromptLibrary — form prompts", () => {
  beforeEach(() => {
    vi.mocked(orchestratorApi.prompts).mockResolvedValue([formPrompt, gitPrompt]);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  async function openOnForm(onInsert = vi.fn()) {
    render(<PromptLibrary currentText="" onInsert={onInsert} />);
    fireEvent.click(screen.getByRole("button", { name: "Open prompt library" }));
    // The form prompt sorts first, so it is the selected entry on open.
    await screen.findByRole("region", { name: "New campaign page form" });
    return onInsert;
  }

  it("shows a form instead of the raw file, with required fields gating Use prompt", async () => {
    const onInsert = await openOnForm();
    // The list marks it as a form; the pane shows fields, not the YAML.
    expect(screen.getByText("· Form")).toBeInTheDocument();
    expect(screen.queryByText(/the raw YAML file/)).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Partner/)).toHaveValue("TWC");

    const use = screen.getByRole("button", { name: "Use prompt" });
    expect(use).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/^Campaign name/), { target: { value: "Go Raw CTV" } });
    expect(use).toBeDisabled(); // the KPI textarea is still required and blank
    fireEvent.change(screen.getByLabelText(/^Channel\(s\) and KPI target/), { target: { value: "CTV, CPM $27" } });
    expect(use).toBeEnabled();

    fireEvent.click(use);
    // Rendered text, not the YAML: the blank optional Deals line is dropped,
    // the toggle renders as no, and the entry's name rides along as before.
    expect(onInsert).toHaveBeenCalledWith(
      "Create a new Pages dashboard (route A).\nPartner: TWC\nCampaign: Go Raw CTV\n" +
        "Channels and KPI targets: CTV, CPM $27\nClient-shareable: no",
      "New campaign page",
    );
    expect(screen.queryByRole("dialog", { name: "Prompt library" })).not.toBeInTheDocument();
  });

  it("tucks optional fields under More options, and keeps a filled one's line", async () => {
    const onInsert = await openOnForm();
    const more = screen.getByRole("button", { name: "More options" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText(/^Deals/)).not.toBeInTheDocument();

    fireEvent.click(more);
    fireEvent.change(screen.getByLabelText(/^Deals/), { target: { value: "PMP-1, PMP-2" } });
    fireEvent.click(screen.getByRole("switch", { name: "Client-shareable" }));
    fireEvent.change(screen.getByLabelText(/^Partner/), { target: { value: "RainBarrel" } });
    fireEvent.change(screen.getByLabelText(/^Campaign name/), { target: { value: "Spring" } });
    fireEvent.change(screen.getByLabelText(/^Channel\(s\) and KPI target/), { target: { value: "Display" } });

    const expected =
      "Create a new Pages dashboard (route A).\nPartner: RainBarrel\nCampaign: Spring\n" +
      "Channels and KPI targets: Display\nDeals: PMP-1, PMP-2\nClient-shareable: yes";
    // The live preview shows exactly what Use prompt will insert.
    expect(screen.getByTestId("prompt-form-preview")).toHaveTextContent(expected, { normalizeWhitespace: false });
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));
    expect(onInsert).toHaveBeenCalledWith(expected, "New campaign page");
  });

  it("inserts the raw template, tokens intact, from Insert raw prompt", async () => {
    const onInsert = await openOnForm();
    // Available without filling anything in: it is the power-user escape hatch.
    fireEvent.click(screen.getByRole("button", { name: "Insert raw prompt" }));
    expect(onInsert).toHaveBeenCalledWith(formPrompt.prompt_template, "New campaign page");
  });

  it("still inserts a plain entry's exact content", async () => {
    const onInsert = await openOnForm();
    fireEvent.click(screen.getByRole("button", { name: /Daily scan/ }));
    expect(screen.queryByRole("region", { name: /form$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Insert raw prompt" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));
    expect(onInsert).toHaveBeenCalledWith(gitPrompt.content, gitPrompt.name);
  });

  it("leaves an untouched optional number out instead of inserting 0", async () => {
    vi.mocked(orchestratorApi.prompts).mockResolvedValue([
      {
        ...formPrompt,
        fields: [
          { key: "campaign", label: "Campaign name", type: "text" as const, required: true },
          { key: "budget", label: "Budget", type: "number" as const, min: 100 },
        ],
        prompt_template: "Campaign: {campaign}\nBudget: {budget}",
      },
    ]);
    const onInsert = await openOnForm();
    expect(screen.getByLabelText(/^Budget/)).toHaveValue(null);
    fireEvent.change(screen.getByLabelText(/^Campaign name/), { target: { value: "Spring" } });
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));
    expect(onInsert).toHaveBeenCalledWith("Campaign: Spring", "New campaign page");
  });

  it("explains a disabled Use prompt when a form of optional fields renders nothing yet", async () => {
    vi.mocked(orchestratorApi.prompts).mockResolvedValue([
      {
        ...formPrompt,
        fields: [{ key: "notes", label: "Notes", type: "textarea" as const }],
        prompt_template: "Notes: {notes}",
      },
    ]);
    await openOnForm();
    const use = screen.getByRole("button", { name: "Use prompt" });
    expect(use).toBeDisabled();
    expect(screen.getByText("Fill in at least one field to use this prompt.")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/^Notes/), { target: { value: "hello" } });
    expect(use).toBeEnabled();
    expect(screen.getByText("Tracked in prompts/new-campaign.yaml")).toBeInTheDocument();
  });

  it("starts each form from its own defaults when switching entries", async () => {
    await openOnForm();
    fireEvent.change(screen.getByLabelText(/^Campaign name/), { target: { value: "Typed" } });
    fireEvent.click(screen.getByRole("button", { name: /Daily scan/ }));
    fireEvent.click(screen.getByRole("button", { name: /New campaign page/ }));
    expect(screen.getByLabelText(/^Campaign name/)).toHaveValue("");
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
