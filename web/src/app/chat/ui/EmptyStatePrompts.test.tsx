import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { EmptyStatePrompts, ProtocolPillForm } from "./EmptyStatePrompts";
import { type ProtocolPill } from "./protocolPills";

// Config-driven fixtures — the shape a pill arrives in over JSON from
// /api/client-config. No client specifics; the component must render whatever
// titles/descs/templates it's handed.
const FORM_PILL: ProtocolPill = {
  id: "report",
  section: "Reporting",
  type: "form",
  icon: "bar-chart",
  title: "Build a report",
  desc: "Fill a few fields and generate a report.",
  cta: "Run report",
  fields: [
    { key: "client", label: "Client name", type: "text", required: true, placeholder: "Acme" },
    { key: "deck", label: "Build a slide deck", type: "toggle", default: false },
  ],
  promptTemplate: "Build a report for {client}.",
};

const CONVERSATION_PILL: ProtocolPill = {
  id: "diagnostic",
  section: "Reporting",
  type: "conversation",
  optionalForm: true,
  icon: "activity",
  title: "Run a diagnostic",
  desc: "Talk it through and dig into the numbers.",
  cta: "Run diagnostic",
  starterPrompt: "I'd like to run a diagnostic. Ask me what you need, then dig in.",
  fields: [{ key: "client", label: "Client name", type: "text" }],
};

// A pill with multi-line inputs — the KPI-list / "anything else" shape.
const TEXTAREA_PILL: ProtocolPill = {
  id: "wrap",
  section: "Reporting",
  type: "form",
  icon: "layers",
  title: "Build a wrap",
  desc: "Summarize a finished campaign.",
  cta: "Build wrap",
  fields: [
    { key: "client", label: "Client name", type: "text", required: true },
    {
      key: "kpis",
      label: "KPIs and goals",
      type: "textarea",
      placeholder: "CTR, goal 0.15%",
      hint: "One per line.",
    },
    { key: "audience", label: "Audience", type: "select", options: ["Client", "Internal"] },
  ],
  promptTemplate: "Wrap {client}.\nKPIs: {kpis}\nAudience: {audience}",
};

const noop = () => {};

function renderForm(pill: ProtocolPill, handlers: Partial<Parameters<typeof ProtocolPillForm>[0]> = {}) {
  return render(
    <ProtocolPillForm
      pill={pill}
      onRun={noop}
      onCancel={noop}
      onDescribe={noop}
      onStartChat={noop}
      {...handlers}
    />,
  );
}

// The prompt-preview body: the <p> that follows the "Prompt preview" kicker.
function previewText(): string {
  const kicker = screen.getByText(/prompt preview/i);
  return kicker.nextElementSibling?.textContent ?? "";
}

describe("EmptyStatePrompts", () => {
  it("renders a card per config-sourced pill and reports the picked id", () => {
    const onPick = vi.fn();
    render(<EmptyStatePrompts pills={[FORM_PILL, CONVERSATION_PILL]} onPick={onPick} />);

    // Titles/descs come straight from the passed pills — nothing hardcoded.
    expect(screen.getByRole("button", { name: /build a report/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /run a diagnostic/i })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /build a report/i }));
    expect(onPick).toHaveBeenCalledWith("report");
  });

  it("renders nothing when given no pills", () => {
    const { container } = render(<EmptyStatePrompts pills={[]} onPick={noop} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("ProtocolPillForm — form pill", () => {
  it("gates Run on required fields, then emits the interpolated template", () => {
    const onRun = vi.fn();
    render(
      <ProtocolPillForm
        pill={FORM_PILL}
        onRun={onRun}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );

    const run = screen.getByRole("button", { name: /run report/i });
    expect(run).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "TestCo" } });
    expect(run).toBeEnabled();

    fireEvent.click(run);
    expect(onRun).toHaveBeenCalledTimes(1);
    expect(onRun.mock.calls[0][0]).toBe("Build a report for TestCo.");
  });

  it("seeds the composer with the generated prompt via the skip-the-form escape hatch", () => {
    const onDescribe = vi.fn();
    render(
      <ProtocolPillForm
        pill={FORM_PILL}
        onRun={noop}
        onCancel={noop}
        onDescribe={onDescribe}
        onStartChat={noop}
      />,
    );

    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "TestCo" } });
    fireEvent.click(screen.getByRole("button", { name: /skip the form, start in chat/i }));
    expect(onDescribe).toHaveBeenCalledTimes(1);
    expect(String(onDescribe.mock.calls[0][0])).toBe("Build a report for TestCo.");
  });

  it("exposes a toggle field (off by default)", () => {
    render(
      <ProtocolPillForm
        pill={FORM_PILL}
        onRun={noop}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );
    const toggle = screen.getByRole("switch", { name: /build a slide deck/i });
    expect(toggle).toHaveAttribute("aria-checked", "false");
  });
});

describe("ProtocolPillForm — text input keeps typed spaces", () => {
  // Regression: the text input rendered `asText(value)`, which trims. Because
  // the input is controlled, "Meridian " was stored with its trailing space but
  // re-rendered as "Meridian", so the space vanished on every keystroke and
  // "Meridian Auto" could never be typed.
  it("round-trips \"Meridian Auto\" through the controlled input, keystroke by keystroke", () => {
    const onRun = vi.fn();
    renderForm(FORM_PILL, { onRun });
    const input = screen.getByLabelText(/client name/i);

    fireEvent.change(input, { target: { value: "Meridian" } });
    expect(input).toHaveValue("Meridian");

    // The keystroke that used to be eaten: a trailing space must survive the
    // re-render so the next character lands after it.
    fireEvent.change(input, { target: { value: "Meridian " } });
    expect(input).toHaveValue("Meridian ");

    fireEvent.change(input, { target: { value: "Meridian Auto" } });
    expect(input).toHaveValue("Meridian Auto");

    // …and the space shows up in the Prompt Preview and the submitted prompt.
    expect(previewText()).toBe("Build a report for Meridian Auto.");
    fireEvent.click(screen.getByRole("button", { name: /run report/i }));
    expect(onRun).toHaveBeenCalledWith("Build a report for Meridian Auto.");
  });

  it("keeps the raw value in the box but trims it once at prompt-assembly time", () => {
    const onRun = vi.fn();
    renderForm(FORM_PILL, { onRun });
    const input = screen.getByLabelText(/client name/i);

    fireEvent.change(input, { target: { value: "Meridian Auto " } });
    expect(input).toHaveValue("Meridian Auto "); // untouched while editing
    expect(previewText()).toBe("Build a report for Meridian Auto."); // trimmed in the prompt

    fireEvent.click(screen.getByRole("button", { name: /run report/i }));
    expect(onRun).toHaveBeenCalledWith("Build a report for Meridian Auto.");
  });
});

describe("ProtocolPillForm — textarea field", () => {
  it("renders a multi-row textarea spanning the full row, with placeholder and hint", () => {
    renderForm(TEXTAREA_PILL);
    const box = screen.getByLabelText(/kpis and goals/i);
    expect(box.tagName).toBe("TEXTAREA");
    expect(box).toHaveAttribute("rows", "4");
    expect(box).toHaveAttribute("placeholder", "CTR, goal 0.15%");
    expect(screen.getByText("One per line.")).toBeInTheDocument();
    // Full row, like text/daterange — not paired up like select/number/toggle.
    expect(box.closest("label")).toHaveClass("sm:col-span-2");
    expect(screen.getByLabelText(/audience/i).closest("label")).not.toHaveClass("sm:col-span-2");
  });

  it("accepts multiple lines and carries them into the preview and the prompt", () => {
    const onRun = vi.fn();
    renderForm(TEXTAREA_PILL, { onRun });
    const multi = "CTR, goal 0.15%\nCPA, spend / conversions, under $40\nUnallocated conversions, report separately";

    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "Meridian Auto" } });
    const box = screen.getByLabelText(/kpis and goals/i);
    fireEvent.change(box, { target: { value: multi } });
    expect(box).toHaveValue(multi); // line breaks survive the controlled re-render

    const expected = `Wrap Meridian Auto.\nKPIs: ${multi}\nAudience: Client`;
    expect(previewText()).toBe(expected);
    // The preview keeps the template's and the value's line breaks visible.
    expect(screen.getByText(/prompt preview/i).nextElementSibling).toHaveClass("whitespace-pre-wrap");

    fireEvent.click(screen.getByRole("button", { name: /build wrap/i }));
    expect(onRun).toHaveBeenCalledWith(expected);
  });

  it("leaves the token in place while the textarea is blank (the agent sees what was intended)", () => {
    renderForm(TEXTAREA_PILL);
    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "Acme" } });
    expect(previewText()).toBe("Wrap Acme.\nKPIs: {kpis}\nAudience: Client");
  });
});

describe("ProtocolPillForm — conversation pill", () => {
  it("routes the skip link to the conversational starter", () => {
    const onStartChat = vi.fn();
    render(
      <ProtocolPillForm
        pill={CONVERSATION_PILL}
        onRun={noop}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={onStartChat}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /skip the form, start in chat/i }));
    expect(onStartChat).toHaveBeenCalledTimes(1);
    expect(String(onStartChat.mock.calls[0][0])).toMatch(/run a diagnostic/i);
  });

  it("falls back to a neutral prompt built from the title + filled fields", () => {
    const onRun = vi.fn();
    render(
      <ProtocolPillForm
        pill={CONVERSATION_PILL}
        onRun={onRun}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );

    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "Acme" } });
    fireEvent.click(screen.getByRole("button", { name: /run diagnostic/i }));
    expect(onRun).toHaveBeenCalledTimes(1);
    // No promptTemplate → neutral "Title. Label: value." fallback.
    expect(onRun.mock.calls[0][0]).toContain("Run a diagnostic.");
    expect(onRun.mock.calls[0][0]).toContain("Client name: Acme");
  });
});
