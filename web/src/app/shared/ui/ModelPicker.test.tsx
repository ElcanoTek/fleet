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
      expect(screen.getByText("OpenAI: GPT-5.6 Sol")).toBeInTheDocument();
    });
    expect(input).toHaveAttribute("aria-expanded", "true");
  });

  it("filters the list as the user types", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox");
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("OpenAI: GPT-5.6 Sol"));
    fireEvent.change(input, { target: { value: "deepseek" } });
    await waitFor(() => {
      expect(screen.getByText("DeepSeek: DeepSeek V4 Flash 0731")).toBeInTheDocument();
    });
    expect(screen.queryByText("OpenAI: GPT-5.6 Sol")).not.toBeInTheDocument();
  });

  it("commits a clicked option into the input value", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox") as HTMLInputElement;
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("DeepSeek: DeepSeek V4 Flash 0731"));
    fireEvent.click(screen.getByText("DeepSeek: DeepSeek V4 Flash 0731"));
    expect(input.value).toBe("deepseek/deepseek-v4-flash-0731");
  });

  it("renders the restaurant-style cost tier for priced catalog models", async () => {
    // Seed-only fallback carries no prices, so this case serves a catalog with
    // pricing: $3/M prompt + $15/M completion blends (3:1) to $6/M → "$$$".
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/model-catalog")) {
          return {
            ok: true,
            json: async () => ({
              models: [
                {
                  slug: "anthropic/claude-sonnet-4.5",
                  name: "Anthropic: Claude Sonnet 4.5",
                  price_prompt: 0.000003,
                  price_completion: 0.000015,
                },
              ],
            }),
          };
        }
        return { ok: true, json: async () => ({ models: [], providers: [] }) };
      }),
    );
    render(<Harness />);
    fireEvent.focus(screen.getByRole("combobox"));
    const indicator = await waitFor(() =>
      screen.getByLabelText(/premium cost — about \$6\.00\/M tokens blended/),
    );
    expect(indicator).toHaveAttribute("data-cost-tier", "3");
    expect(indicator).toHaveTextContent("$$$$");
  });

  it("omits the cost tier for models with no known pricing", async () => {
    // Seed fallback (fetch rejects) — no prices anywhere, so no glyphs.
    render(<Harness />);
    fireEvent.focus(screen.getByRole("combobox"));
    await waitFor(() => screen.getByText("DeepSeek: DeepSeek V4 Flash 0731"));
    expect(document.querySelectorAll(".model-cost")).toHaveLength(0);
  });

  it("shows the 'type a custom slug' empty state for no matches", async () => {
    render(<Harness />);
    const input = screen.getByRole("combobox");
    fireEvent.focus(input);
    await waitFor(() => screen.getByText("OpenAI: GPT-5.6 Sol"));
    fireEvent.change(input, { target: { value: "zzz-nope" } });
    await waitFor(() => {
      expect(
        screen.getByText("No matching models — type a custom slug to use it."),
      ).toBeInTheDocument();
    });
  });
});
