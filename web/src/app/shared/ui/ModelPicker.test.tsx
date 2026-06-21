import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { useState } from "react";
import { ModelPicker } from "./ModelPicker";
import { _resetModelCacheForTests } from "@/app/shared/lib/models";

// Component test for the React ModelPicker (port of moc's model-picker.js).
// The pure filtering logic is covered in models.test.ts; here we verify the
// combobox UI: opens on focus (browse mode), filters on input, and commits a
// clicked option.

function Harness() {
  const [value, setValue] = useState("");
  return <ModelPicker value={value} onChange={setValue} placeholder="model slug" />;
}

describe("ModelPicker", () => {
  beforeEach(() => {
    _resetModelCacheForTests();
    // Fail the network fetch so the picker falls back to the seed list — keeps
    // the test deterministic without hitting OpenRouter.
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("no network in test")));
  });
  afterEach(() => {
    vi.restoreAllMocks();
    cleanup();
  });

  it("opens the listbox and shows seed models on focus", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox");
    fireEvent.focus(input);
    await waitFor(() => {
      expect(screen.getByText("Anthropic: Claude Opus 4.8")).toBeInTheDocument();
    });
    expect(input).toHaveAttribute("aria-expanded", "true");
  });

  it("filters the list as the user types", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox");
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("Anthropic: Claude Opus 4.8"));
    fireEvent.change(input, { target: { value: "gemini" } });
    await waitFor(() => {
      expect(screen.getByText("Google: Gemini 3.5 Flash")).toBeInTheDocument();
    });
    expect(screen.queryByText("Anthropic: Claude Opus 4.8")).not.toBeInTheDocument();
  });

  it("commits a clicked option into the input value", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox") as HTMLInputElement;
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("MoonshotAI: Kimi K2.6"));
    fireEvent.click(screen.getByText("MoonshotAI: Kimi K2.6"));
    expect(input.value).toBe("moonshotai/kimi-k2.6");
  });

  it("shows the 'type a custom slug' empty state for no matches", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox");
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("Anthropic: Claude Opus 4.8"));
    fireEvent.change(input, { target: { value: "zzz-nope" } });
    await waitFor(() => {
      expect(
        screen.getByText("No matching models — type a custom slug to use it."),
      ).toBeInTheDocument();
    });
  });
});
