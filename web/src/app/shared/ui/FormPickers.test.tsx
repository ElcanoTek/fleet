import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterAll, afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FormFields } from "./FormFields";
import { PromptLibraryForm, type FormPromptItem } from "./PromptLibraryForm";
import type { PillField, PillFieldValue, PillValues } from "@/app/chat/ui/protocolPills";
import { setBootstrapForTest } from "@/app/lib/bootstrap";

const recipientHistory = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: { recipientHistory: (...args: unknown[]) => recipientHistory(...args) },
}));

const to: PillField = { key: "to", label: "Send to", type: "recipients", hint: "Leave empty for a review copy." };
const style: PillField = {
  key: "style",
  label: "Style",
  type: "palette",
  options: ["elcano", "amazon"],
  swatches: { elcano: ["#1A0B1E", "#6262A0"], amazon: ["#333E48", "#FF9900"] },
  custom: ["Header", "Accent"],
};

function Harness({ fields, initial }: { fields: PillField[]; initial: PillValues }) {
  const [values, setValues] = useState(initial);
  return (
    <>
      <FormFields
        fields={fields}
        values={values}
        onChange={(key: string, value: PillFieldValue) => setValues((prev) => ({ ...prev, [key]: value }))}
      />
      <output data-testid="values">{JSON.stringify(values)}</output>
    </>
  );
}

const values = () => JSON.parse(screen.getByTestId("values").textContent ?? "{}") as Record<string, string>;

beforeEach(() => {
  window.localStorage.clear();
  setBootstrapForTest({ session: { email: "me@elcanotek.com" } });
  recipientHistory.mockReset();
  recipientHistory.mockResolvedValue([
    { email: "rare@client.com", task_count: 1, last_used: "2026-11-01T00:00:00Z" },
    { email: "Team@client.com", task_count: 7, last_used: "2026-11-10T00:00:00Z" },
  ]);
});
afterEach(cleanup);
afterAll(() => setBootstrapForTest(null));

describe("RecipientPicker", () => {
  it("offers the most used addresses first and adds one with a click", async () => {
    render(<Harness fields={[to]} initial={{ to: "" }} />);
    const offered = await screen.findByTestId("recipient-suggestions");
    await waitFor(() => expect(within(offered).getAllByRole("button")).toHaveLength(2));
    expect(within(offered).getAllByRole("button").map((b) => b.textContent)).toEqual(["team@client.com", "rare@client.com"]);

    fireEvent.click(within(offered).getByRole("button", { name: "team@client.com" }));
    expect(values().to).toBe("team@client.com");
    expect(within(screen.getByTestId("recipient-chosen")).getByText("team@client.com")).toBeTruthy();
    // A chosen address is no longer offered.
    expect(within(screen.getByTestId("recipient-suggestions")).queryByRole("button", { name: "team@client.com" })).toBeNull();
  });

  it("adds a typed address, remembers it for next time, and removes it again", async () => {
    const first = render(<Harness fields={[to]} initial={{ to: "" }} />);
    const box = screen.getByPlaceholderText("Add an address");
    fireEvent.change(box, { target: { value: "New.Person@client.com" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(values().to).toBe("new.person@client.com");

    fireEvent.click(screen.getByRole("button", { name: "Remove new.person@client.com" }));
    expect(values().to).toBe("");
    first.unmount();

    recipientHistory.mockResolvedValue([]);
    render(<Harness fields={[to]} initial={{ to: "" }} />);
    expect(within(screen.getByTestId("recipient-suggestions")).getByRole("button", { name: "new.person@client.com" })).toBeTruthy();
  });

  it("narrows the suggestions as you type, without complaining about the fragment", async () => {
    render(<Harness fields={[to]} initial={{ to: "" }} />);
    const offered = await screen.findByTestId("recipient-suggestions");
    await waitFor(() => expect(within(offered).getAllByRole("button")).toHaveLength(2));
    const box = screen.getByPlaceholderText("Add an address");
    fireEvent.change(box, { target: { value: "rar" } });
    expect(within(offered).getAllByRole("button").map((b) => b.textContent)).toEqual(["rare@client.com"]);
    fireEvent.blur(box);
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.click(within(offered).getByRole("button", { name: "rare@client.com" }));
    expect(values().to).toBe("rare@client.com");
    expect((box as HTMLInputElement).value).toBe("");
  });

  it("refuses something that is not an address and says why", () => {
    recipientHistory.mockResolvedValue([]);
    render(<Harness fields={[to]} initial={{ to: "a@x.com" }} />);
    const box = screen.getByPlaceholderText("Add an address");
    fireEvent.change(box, { target: { value: "not an address" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByRole("alert").textContent).not.toBe("");
    expect(values().to).toBe("a@x.com");
  });

  it("lists the bundle's own suggestions and works when history cannot be read", async () => {
    recipientHistory.mockRejectedValue(new Error("offline"));
    render(<Harness fields={[{ ...to, options: ["desk@client.com"] }]} initial={{ to: "" }} />);
    const offered = await screen.findByTestId("recipient-suggestions");
    expect(within(offered).getAllByRole("button").map((b) => b.textContent)).toEqual(["desk@client.com"]);
  });
});

describe("PalettePicker", () => {
  it("draws each option with its colors and selects one", () => {
    render(<Harness fields={[style]} initial={{ style: "elcano" }} />);
    const group = screen.getByRole("radiogroup", { name: "Style" });
    const radios = within(group).getAllByRole("radio");
    expect(radios.map((r) => r.textContent)).toEqual(["elcano", "amazon", "Custom colors"]);
    expect(radios[0].getAttribute("aria-checked")).toBe("true");

    fireEvent.click(radios[1]);
    expect(values().style).toBe("amazon");
    expect(screen.queryByTestId("palette-custom")).toBeNull();
  });

  it("lets the person pick the colors themselves", () => {
    render(<Harness fields={[style]} initial={{ style: "elcano" }} />);
    fireEvent.click(screen.getByRole("radio", { name: "Custom colors" }));
    expect(values().style).toBe("custom (Header #1F3A5F, Accent #1F3A5F)");

    fireEvent.change(screen.getByLabelText("Accent"), { target: { value: "#c2410c" } });
    expect(values().style).toBe("custom (Header #1F3A5F, Accent #C2410C)");

    // A detour to a preset keeps the picked colors.
    fireEvent.click(screen.getByRole("radio", { name: "amazon" }));
    fireEvent.click(screen.getByRole("radio", { name: "Custom colors" }));
    expect(values().style).toBe("custom (Header #1F3A5F, Accent #C2410C)");
  });

  it("offers the looks used last, and no custom choice when the field has none", () => {
    window.localStorage.setItem(
      "fleet.form.palette:me@elcanotek.com:style",
      JSON.stringify(["custom (Header #0B3D2E, Accent #C2410C)", "amazon", "retired"]),
    );
    render(<Harness fields={[style]} initial={{ style: "elcano" }} />);
    const recent = within(screen.getByTestId("palette-recent")).getAllByRole("button");
    expect(recent.map((b) => b.textContent)).toEqual(["Custom", "amazon"]);
    fireEvent.click(recent[0]);
    expect(values().style).toBe("custom (Header #0B3D2E, Accent #C2410C)");
    expect((screen.getByLabelText("Header") as HTMLInputElement).value).toBe("#0b3d2e");
    cleanup();

    render(<Harness fields={[{ ...style, custom: undefined, key: "plain" }]} initial={{ plain: "elcano" }} />);
    expect(screen.queryByRole("radio", { name: "Custom colors" })).toBeNull();
    expect(screen.queryByTestId("palette-recent")).toBeNull();
  });
});

describe("a form with pickers", () => {
  const prompt = {
    name: "Weekly update",
    path: "prompts/weekly.yaml",
    content: "",
    fields: [{ key: "campaign", label: "Campaign", type: "text", required: true }, to, style],
    prompt_template: "Write the update for {campaign}.\nSend to: {to}\nStyle: {style}",
  } as unknown as FormPromptItem;

  it("renders the picked values into the prompt and remembers them once used", async () => {
    const onUse = vi.fn();
    render(<PromptLibraryForm prompt={prompt} note="" onUse={onUse} />);
    fireEvent.change(screen.getByLabelText(/Campaign/), { target: { value: "NorthPeak" } });
    // No recipient yet: its line is dropped; the style always has a value.
    expect(screen.getByTestId("prompt-form-preview").textContent).toBe("Write the update for NorthPeak.\nStyle: elcano");

    const offered = await screen.findByTestId("recipient-suggestions");
    fireEvent.click(within(offered).getByRole("button", { name: "team@client.com" }));
    fireEvent.click(screen.getByRole("radio", { name: "amazon" }));
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));

    expect(onUse).toHaveBeenCalledWith("Write the update for NorthPeak.\nSend to: team@client.com\nStyle: amazon");
    expect(JSON.parse(window.localStorage.getItem("fleet.form.palette:me@elcanotek.com:style") ?? "[]")).toEqual(["amazon"]);
    expect(JSON.parse(window.localStorage.getItem("fleet.form.recipients:me@elcanotek.com") ?? "[]")).toEqual(["team@client.com"]);
  });
});
