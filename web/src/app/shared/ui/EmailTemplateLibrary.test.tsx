import { cleanup, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  defaultTemplatePrompt,
  EmailTemplateLibrary,
  resetEmailTemplateAvailabilityForTests,
  useHasEmailTemplates,
} from "./EmailTemplateLibrary";
import { orchestratorApi, type EmailTemplate } from "@/app/shared/lib/orchestratorApi";

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: { emailTemplates: vi.fn() },
}));

const weekly: EmailTemplate = {
  id: "weekly-optimization-update",
  name: "Weekly optimization update",
  description: "The trader's weekly client note.",
  audience: "client",
  use_when: "A campaign has DSP reporting and an optimization log.",
  path: "email_templates/weekly-optimization-update.yaml",
  max_words: 260,
  subject: { instruction: "Campaign, channel, the one result.", max_chars: 70 },
  rules: ["Write for a client who gives this one minute."],
  never_say: ["no optimizations", "re:\\bELC\\d{3,}\\b"],
  sections: [
    {
      id: "summary",
      title: "Summary",
      block: "text",
      required: true,
      instruction: "Greeting, one framing sentence, up to three bullets.",
      limits: { max_paragraphs: 2, max_bullets: 3, max_words: 100 },
    },
    { id: "trend", title: "Trend chart", block: "chart", required: false, instruction: "One chart." },
  ],
  preview_html: "<!doctype html><html><body><p>Hi Team,</p></body></html>",
  fields: [
    { key: "campaign", label: "Campaign", type: "text", required: true },
    { key: "notes", label: "Notes", type: "textarea", advanced: true },
  ],
  prompt_template: "Write this week's update for {campaign} with the email template weekly-optimization-update.\nNotes: {notes}",
};

const brief: EmailTemplate = {
  id: "internal-brief",
  name: "Internal brief",
  audience: "internal",
  path: "email_templates/internal-brief.yaml",
  subject: {},
  sections: [{ id: "flags", title: "Red flags", block: "text", required: true }],
};

function open(onInsert = vi.fn()) {
  render(
    <EmailTemplateLibrary
      onInsert={onInsert}
      renderTrigger={(openLibrary) => (
        <button type="button" onClick={openLibrary}>
          open
        </button>
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "open" }));
  return onInsert;
}

describe("EmailTemplateLibrary", () => {
  beforeEach(() => {
    resetEmailTemplateAvailabilityForTests();
    vi.mocked(orchestratorApi.emailTemplates).mockResolvedValue([weekly, brief]);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("shows the example in a scriptless frame beside each section's rules", async () => {
    open();
    const dialog = await screen.findByRole("dialog", { name: "Email templates" });
    const frame = await within(dialog).findByTitle("Example of Weekly optimization update");
    expect(frame).toHaveAttribute("sandbox", "");
    expect(frame.getAttribute("srcdoc")).toContain("Hi Team,");

    const sections = within(dialog).getByTestId("email-template-sections");
    expect(within(sections).getByText("1. Summary")).toBeInTheDocument();
    expect(within(sections).getByText("Up to 100 words · Up to 2 paragraphs · Up to 3 bullets")).toBeInTheDocument();
    expect(within(sections).getByText("Optional")).toBeInTheDocument();
    expect(within(dialog).getByText("For clients")).toBeInTheDocument();
    expect(within(dialog).getByText(/Up to 260 words of prose/)).toBeInTheDocument();
    // A phrase is shown; a pattern is described, never printed.
    expect(within(dialog).getByText("no optimizations")).toBeInTheDocument();
    expect(within(dialog).getByText("internal codes and IDs")).toBeInTheDocument();
    expect(within(dialog).queryByText(/ELC\\d/)).not.toBeInTheDocument();
  });

  it("fills the form and inserts the prompt, dropping an unanswered optional line", async () => {
    const onInsert = open();
    const dialog = await screen.findByRole("dialog", { name: "Email templates" });
    // The rules come first; the form replaces them once the template is used.
    expect(within(dialog).queryByRole("button", { name: "Use prompt" })).not.toBeInTheDocument();
    fireEvent.click(await within(dialog).findByRole("button", { name: "Use this template" }));
    expect(within(dialog).queryByTestId("email-template-sections")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Back to the rules" }));
    expect(within(dialog).getByTestId("email-template-sections")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Use this template" }));
    const use = within(dialog).getByRole("button", { name: "Use prompt" });
    expect(use).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText(/Campaign/), { target: { value: "Acme CTV" } });
    fireEvent.click(use);
    expect(onInsert).toHaveBeenCalledWith(
      "Write this week's update for Acme CTV with the email template weekly-optimization-update.",
    );
    expect(screen.queryByRole("dialog", { name: "Email templates" })).not.toBeInTheDocument();
  });

  it("uses a default prompt for a template without a form or an example", async () => {
    const onInsert = open();
    const dialog = await screen.findByRole("dialog", { name: "Email templates" });
    fireEvent.click(await within(dialog).findByRole("button", { name: /Internal brief/ }));
    expect(within(dialog).getByText(/has no example to show/)).toBeInTheDocument();
    expect(within(dialog).getByText("Internal only")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Use this template" }));
    expect(onInsert).toHaveBeenCalledWith(defaultTemplatePrompt(brief));
    expect(defaultTemplatePrompt(brief)).toContain('"internal-brief"');
  });

  it("filters by search and says when the workspace has none", async () => {
    open();
    const dialog = await screen.findByRole("dialog", { name: "Email templates" });
    await within(dialog).findByRole("button", { name: /Internal brief/ });
    fireEvent.change(within(dialog).getByRole("searchbox", { name: "Search email templates" }), {
      target: { value: "optimization" },
    });
    expect(within(dialog).queryByRole("button", { name: /Internal brief/ })).not.toBeInTheDocument();
    cleanup();

    vi.mocked(orchestratorApi.emailTemplates).mockResolvedValue([]);
    open();
    expect(await screen.findByText("This workspace has no email templates yet.")).toBeInTheDocument();
  });

  it("reports a load failure", async () => {
    vi.mocked(orchestratorApi.emailTemplates).mockRejectedValue(new Error("boom"));
    open();
    expect(await screen.findByText("boom")).toBeInTheDocument();
  });
});

describe("useHasEmailTemplates", () => {
  beforeEach(() => resetEmailTemplateAvailabilityForTests());
  afterEach(() => vi.clearAllMocks());

  it("asks only once it is told to, and shares one answer", async () => {
    vi.mocked(orchestratorApi.emailTemplates).mockResolvedValue([brief]);
    const first = renderHook(({ ask }) => useHasEmailTemplates(ask), { initialProps: { ask: false } });
    expect(first.result.current).toBe(false);
    expect(orchestratorApi.emailTemplates).not.toHaveBeenCalled();
    first.rerender({ ask: true });
    await waitFor(() => expect(first.result.current).toBe(true));
    const second = renderHook(() => useHasEmailTemplates(true));
    await waitFor(() => expect(second.result.current).toBe(true));
    expect(orchestratorApi.emailTemplates).toHaveBeenCalledTimes(1);
  });

  it("stays off for an empty library and asks again after a failure", async () => {
    vi.mocked(orchestratorApi.emailTemplates).mockRejectedValueOnce(new Error("down")).mockResolvedValue([]);
    const failed = renderHook(() => useHasEmailTemplates(true));
    await waitFor(() => expect(orchestratorApi.emailTemplates).toHaveBeenCalledTimes(1));
    expect(failed.result.current).toBe(false);
    const again = renderHook(() => useHasEmailTemplates(true));
    await waitFor(() => expect(orchestratorApi.emailTemplates).toHaveBeenCalledTimes(2));
    expect(again.result.current).toBe(false);
  });
});
