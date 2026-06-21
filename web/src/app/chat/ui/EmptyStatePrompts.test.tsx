import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { EmptyStatePrompts, ProtocolPillForm } from "./EmptyStatePrompts";
import { getPill, PROTOCOL_PILLS, type ProtocolPill } from "./protocolPills";

function pill(id: string): ProtocolPill {
  const p = getPill(id);
  if (!p) throw new Error(`missing pill: ${id}`);
  return p;
}

const noop = () => {};

describe("EmptyStatePrompts", () => {
  it("renders a card per pill and reports the picked id", () => {
    const onPick = vi.fn();
    render(<EmptyStatePrompts pills={PROTOCOL_PILLS} onPick={onPick} />);

    expect(screen.getByRole("button", { name: /weekly performance report/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /performance diagnostic/i })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /optimization report/i }));
    expect(onPick).toHaveBeenCalledWith("optimization");
  });
});

describe("ProtocolPillForm — form pill", () => {
  it("gates Run on required fields, then emits the templated prompt", () => {
    const onRun = vi.fn();
    render(
      <ProtocolPillForm
        pill={pill("weekly")}
        onRun={onRun}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );

    const run = screen.getByRole("button", { name: /run report/i });
    expect(run).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "TestCo" } });
    fireEvent.change(screen.getByLabelText(/campaign code/i), { target: { value: "ELC-9999" } });
    expect(run).toBeEnabled();

    fireEvent.click(run);
    expect(onRun).toHaveBeenCalledTimes(1);
    expect(onRun.mock.calls[0][0]).toContain("Run the DSP reporting protocol for TestCo (ELC-9999).");
  });

  it("seeds the composer via the skip-the-form escape hatch", () => {
    const onDescribe = vi.fn();
    render(
      <ProtocolPillForm
        pill={pill("weekly")}
        onRun={noop}
        onCancel={noop}
        onDescribe={onDescribe}
        onStartChat={noop}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /skip the form, start in chat/i }));
    expect(onDescribe).toHaveBeenCalledTimes(1);
    expect(String(onDescribe.mock.calls[0][0])).toMatch(/weekly performance report/i);
  });

  it("exposes the Gamma deck toggle (off by default) on the wrap pill", () => {
    render(
      <ProtocolPillForm
        pill={pill("wrap")}
        onRun={noop}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );
    const toggle = screen.getByRole("switch", { name: /gamma slide deck/i });
    expect(toggle).toHaveAttribute("aria-checked", "false");
  });
});

describe("ProtocolPillForm — conversation pill", () => {
  it("routes the skip link to the conversational starter", () => {
    const onStartChat = vi.fn();
    render(
      <ProtocolPillForm
        pill={pill("diagnostic")}
        onRun={noop}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={onStartChat}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /skip the form, start in chat/i }));
    expect(onStartChat).toHaveBeenCalledTimes(1);
    expect(String(onStartChat.mock.calls[0][0])).toMatch(/performance diagnostic/i);
  });

  it("shows the form up front and folds the split campaign fields into the prompt", () => {
    const onRun = vi.fn();
    render(
      <ProtocolPillForm
        pill={pill("diagnostic")}
        onRun={onRun}
        onCancel={noop}
        onDescribe={noop}
        onStartChat={noop}
      />,
    );

    // The form is the primary surface now — no disclosure to expand first.
    fireEvent.change(screen.getByLabelText(/client name/i), { target: { value: "Acme" } });
    fireEvent.change(screen.getByLabelText(/campaign code/i), { target: { value: "ELC-1" } });

    fireEvent.click(screen.getByRole("button", { name: /run diagnostic/i }));
    expect(onRun).toHaveBeenCalledTimes(1);
    expect(onRun.mock.calls[0][0]).toContain("Campaign: Acme (ELC-1)");
  });
});
