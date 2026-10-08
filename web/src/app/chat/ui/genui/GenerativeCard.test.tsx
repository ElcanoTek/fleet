import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import GenerativeCard, { RENDERERS, parseListText } from "./GenerativeCard";
import { parseCardSpec, parseSubmissionMessage, type CardSpec } from "./model";
import { loadFixture } from "./fixtures";

type Case = { name: string; valid: boolean; card: unknown };

beforeEach(() => {
  try {
    window.localStorage.clear();
  } catch {
    /* ignore */
  }
});

function spec(o: unknown): CardSpec {
  const s = parseCardSpec(JSON.stringify(o));
  if (!s) throw new Error("bad spec");
  return s;
}

describe("catalog parity", () => {
  it("renders exactly the components the validator accepts", () => {
    const catalog = loadFixture<string[]>("catalog.json");
    expect(Object.keys(RENDERERS).sort()).toEqual([...catalog].sort());
  });
});

describe("every valid fixture card renders", () => {
  const { cases } = loadFixture<{ cases: Case[] }>("cards.json");
  for (const c of cases.filter((x) => x.valid)) {
    it(c.name, () => {
      const s = spec(c.card);
      const { container } = render(<GenerativeCard cardId="card_1" spec={s} onSubmit={() => {}} />);
      expect(screen.getByTestId("genui-card")).toBeTruthy();
      expect(container.textContent).toContain(s.title);
      expect(container.textContent).not.toContain("⚠");
      // Nothing model-authored becomes an <img> or <script>.
      expect(container.querySelector("img,script,iframe")).toBeNull();
    });
  }
});

const form = {
  title: "Order",
  components: [
    { type: "text_input", id: "name", label: "Name", required: true },
    { type: "number", id: "qty", label: "Quantity", value: 2 },
    { type: "number", id: "price", label: "Price", value: 5 },
    { type: "stat", label: "Total", value: "{{ qty * price }}" },
    { type: "toggle", id: "gift", label: "Gift wrap" },
    { type: "text_input", id: "note", label: "Gift note", visible_if: "gift" },
  ],
  actions: [
    { id: "order", label: "Place order", confirm: "Place it now?" },
    { id: "nah", label: "Not now", kind: "message", message: "Skip the order." },
  ],
};

describe("interaction", () => {
  it("recomputes templates live as inputs change", async () => {
    const user = userEvent.setup();
    render(<GenerativeCard cardId="c" spec={spec(form)} onSubmit={() => {}} />);
    expect(screen.getByTestId("genui-stat-value").textContent).toBe("10");
    const qty = screen.getByLabelText("Quantity");
    await user.clear(qty);
    await user.type(qty, "4");
    expect(screen.getByTestId("genui-stat-value").textContent).toBe("20");
  });

  it("shows conditional fields only when their condition holds", async () => {
    const user = userEvent.setup();
    render(<GenerativeCard cardId="c" spec={spec(form)} onSubmit={() => {}} />);
    expect(screen.queryByLabelText("Gift note")).toBeNull();
    await user.click(screen.getByRole("switch"));
    expect(screen.getByLabelText("Gift note")).toBeTruthy();
  });

  it("blocks submit on a missing required field, then confirms and sends the values", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="call_7" spec={spec(form)} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Required")).toBeTruthy();
    expect(screen.getByRole("status").textContent).toContain("Fix 1 field");

    await user.type(screen.getByLabelText(/Name/), "Ada");
    // The "fix N fields" notice clears as soon as the user edits.
    expect(screen.queryByRole("status")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Place it now?")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Yes, place order" }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const sub = parseSubmissionMessage(onSubmit.mock.calls[0][0]);
    // The hidden gift note is not submitted.
    expect(sub).toEqual({ cardId: "call_7", actionId: "order", values: { name: "Ada", qty: 2, price: 5, gift: false } });
    expect(await screen.findByTestId("genui-submitted")).toBeTruthy();
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
  });

  it("a message action sends its fixed text without validating", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="c" spec={spec(form)} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Not now" }));
    expect(onSubmit).toHaveBeenCalledWith("Skip the order.");
  });

  it("server field_errors show until that field is edited", async () => {
    const user = userEvent.setup();
    const s = spec({ ...form, field_errors: [{ field: "name", message: "That name is taken" }] });
    render(<GenerativeCard cardId="c" spec={s} onSubmit={() => {}} />);
    expect(screen.getByText("That name is taken")).toBeTruthy();
    await user.type(screen.getByLabelText(/Name/), "B");
    expect(screen.queryByText("That name is taken")).toBeNull();
  });

  it("restores a submitted card read-only with the submitted values, and Edit unlocks it", async () => {
    const user = userEvent.setup();
    render(
      <GenerativeCard
        cardId="c"
        spec={spec(form)}
        submission={{ cardId: "c", actionId: "order", values: { name: "Grace", qty: 3 } }}
        onSubmit={() => {}}
      />,
    );
    const name = screen.getByLabelText(/Name/) as HTMLInputElement;
    expect(name.value).toBe("Grace");
    expect(name).toBeDisabled();
    expect(screen.getByTestId("genui-submitted").textContent).toContain("Place order");
    expect(screen.getByTestId("genui-stat-value").textContent).toBe("15");
    await user.click(screen.getByRole("button", { name: "Edit and resend" }));
    expect(screen.getByLabelText(/Name/)).toBeEnabled();
  });

  it("collapses a replaced card", () => {
    render(<GenerativeCard cardId="c" spec={spec(form)} superseded onSubmit={() => {}} />);
    expect(screen.getByTestId("genui-card").getAttribute("data-state")).toBe("superseded");
    expect(screen.queryByLabelText(/Name/)).toBeNull();
  });

  it("keeps a draft across a remount (the transcript is virtualized)", async () => {
    const user = userEvent.setup();
    const s = spec(form);
    const first = render(<GenerativeCard cardId="draft_card" spec={s} onSubmit={() => {}} />);
    await user.type(screen.getByLabelText(/Name/), "Lin");
    first.unmount();
    render(<GenerativeCard cardId="draft_card" spec={s} onSubmit={() => {}} />);
    expect((screen.getByLabelText(/Name/) as HTMLInputElement).value).toBe("Lin");
  });
});

describe("repeater", () => {
  const rep = {
    title: "Lines",
    components: [
      {
        type: "repeater",
        id: "lines",
        label: "Lines",
        item_label: "Line {{ index }}: {{ channel }}",
        max_items: 3,
        fields: [{ type: "choice", id: "channel", label: "Channel", options: ["Display", "CTV"], required: true }],
        value: [{ channel: "CTV" }],
      },
      { type: "stat", label: "Count", value: "{{ count(lines) }}" },
    ],
    actions: [{ id: "go", label: "Go" }],
  };

  it("adds, duplicates and removes items, recomputing aggregates", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="r" spec={spec(rep)} onSubmit={onSubmit} />);
    expect(screen.getByText("Line 1: CTV")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Duplicate" }));
    expect(screen.getByTestId("genui-stat-value").textContent).toBe("2");
    await user.click(screen.getByRole("button", { name: "+ Add" }));
    expect(screen.getByTestId("genui-stat-value").textContent).toBe("3");
    // At max_items the add button goes away.
    expect(screen.queryByRole("button", { name: "+ Add" })).toBeNull();
    // The new third item has no channel: submit is blocked on its path.
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    const third = document.querySelector('[data-repeater-item="2"]') as HTMLElement;
    expect(within(third).getByText("Required")).toBeTruthy();
    await user.click(within(third).getAllByRole("button", { name: "Remove" })[0]);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values).toEqual({
      lines: [{ channel: "CTV" }, { channel: "CTV" }],
    });
  });
});

describe("safety", () => {
  it("never renders a non-https link as a link", () => {
    render(<GenerativeCard cardId="s" spec={spec({ title: "x", components: [{ type: "link", text: "click", url: "javascript:alert(1)" }] })} />);
    expect(screen.getByText("click").closest("a")).toBeNull();
  });

  it("markdown drops images and raw HTML", () => {
    const { container } = render(
      <GenerativeCard
        cardId="s"
        spec={spec({
          title: "x",
          components: [{ type: "text", markdown: true, text: "![x](https://evil.example/p.png) <img src=x onerror=alert(1)> [ok](https://example.com) [bad](javascript:alert(1))" }],
        })}
      />,
    );
    expect(container.querySelector("img")).toBeNull();
    const links = [...container.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(links).toEqual(["https://example.com"]);
    expect(container.querySelector("a")?.getAttribute("rel")).toContain("noopener");
  });

  it("skips an unknown component instead of throwing", () => {
    render(<GenerativeCard cardId="s" spec={spec({ title: "x", components: [{ type: "marquee" }, { type: "text", text: "still here" }] })} />);
    expect(screen.getByText("still here")).toBeTruthy();
  });

  it("without onSubmit (read-only) every action is disabled", () => {
    render(<GenerativeCard cardId="s" spec={spec(form)} readOnly />);
    expect(screen.queryByRole("button", { name: "Place order" })).toBeNull();
  });
});

describe("include/exclude lanes and list input", () => {
  it("moves an item between lanes and submits both sides", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "Geo",
      components: [{ type: "include_exclude", id: "geo", label: "Geo", options: ["US", "CA", "MX"], value: { include: ["US"], exclude: [] } }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="g" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Move US to Exclude" }));
    await user.click(screen.getByRole("button", { name: "+ CA" }));
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Go" }));
    });
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values).toEqual({ geo: { include: ["CA"], exclude: ["US"] } });
  });

  it("parses pasted lists one per line, trimmed and de-duplicated", () => {
    expect(parseListText(" a.com\r\nb.com\n\na.com \n", true)).toEqual(["a.com", "b.com"]);
    expect(parseListText("a\na", false)).toEqual(["a", "a"]);
  });
});
