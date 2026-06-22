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

const noop = () => {};

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
