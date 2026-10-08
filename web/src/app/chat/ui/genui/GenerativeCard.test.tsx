import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import GenerativeCard, { RENDERERS, parseListText } from "./GenerativeCard";
import { buildReplyMessage, parseCardSpec, parseSubmissionMessage, type CardSpec } from "./model";
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
  });

  it("locks only once the submission is in the transcript; a refused send stays editable with its draft", async () => {
    const user = userEvent.setup();
    const s = spec(form);
    // submitPrompt resolves normally even when the server refused the turn.
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    const view = render(<GenerativeCard cardId="lock_card" spec={s} onSubmit={onSubmit} />);
    await user.type(screen.getByLabelText(/Name/), "Ada");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    await user.click(screen.getByRole("button", { name: "Yes, place order" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    // No submission in the transcript yet: still editable, draft kept.
    expect(screen.queryByTestId("genui-submitted")).toBeNull();
    expect(screen.getByLabelText(/Name/)).toBeEnabled();
    expect(window.localStorage.getItem("fleet.genui.draft.lock_card")).toContain("Ada");
    // The user message lands: the card locks and the draft is dropped.
    const submission = parseSubmissionMessage(onSubmit.mock.calls[0][0]);
    view.rerender(<GenerativeCard cardId="lock_card" spec={s} submission={submission} onSubmit={onSubmit} />);
    expect(await screen.findByTestId("genui-submitted")).toBeTruthy();
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
    expect(window.localStorage.getItem("fleet.genui.draft.lock_card")).toBeNull();
  });

  it("a message action sends its fixed text without validating", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="c" spec={spec(form)} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Not now" }));
    // A quick reply carries a marker naming its card, then the fixed text.
    expect(onSubmit).toHaveBeenCalledWith(buildReplyMessage("c", "nah", "Skip the order."));
  });

  it("re-checks the gates at confirmation time", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="c" spec={spec(form)} onSubmit={onSubmit} />);
    await user.type(screen.getByLabelText(/Name/), "Ada");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    // The card is still editable while the confirmation is open.
    await user.clear(screen.getByLabelText(/Name/));
    await user.click(screen.getByRole("button", { name: "Yes, place order" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Required")).toBeTruthy();
  });

  it("an unedited server field error blocks submit until the user changes that field", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      ...form,
      components: form.components.map((c) => (c.id === "name" ? { ...c, value: "Taken" } : c)),
      actions: [{ id: "order", label: "Place order" }],
      field_errors: [{ field: "name", message: "That name is taken" }],
    });
    render(<GenerativeCard cardId="c" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByRole("status").textContent).toContain("Fix 1 field");
    await user.type(screen.getByLabelText(/Name/), "2");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values.name).toBe("Taken2");
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
    // Cancelling the edit puts back exactly what was sent.
    await user.clear(screen.getByLabelText(/Name/));
    await user.type(screen.getByLabelText(/Name/), "Changed");
    await user.click(screen.getByRole("button", { name: "Cancel edit" }));
    expect((screen.getByLabelText(/Name/) as HTMLInputElement).value).toBe("Grace");
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
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

describe("second Codex pass", () => {
  it("an accepted submit waits for the transcript with actions disabled; a refused one says so", async () => {
    const user = userEvent.setup();
    const accepted = vi.fn().mockResolvedValue(true);
    const s = spec({ ...form, actions: [{ id: "order", label: "Place order" }] });
    const view = render(<GenerativeCard cardId="q" spec={s} onSubmit={accepted} />);
    await user.type(screen.getByLabelText(/Name/), "Ada");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(await screen.findByTestId("genui-awaiting")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Place order" })).toBeNull();
    view.unmount();

    const refused = vi.fn().mockResolvedValue(false);
    render(<GenerativeCard cardId="q2" spec={s} onSubmit={refused} />);
    await user.type(screen.getByLabelText(/Name/), "Ada");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(await screen.findByText("Not sent. Try again.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Place order" })).toBeEnabled();
  });

  it("a server error on a field the user hid does not block submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      ...form,
      components: form.components.map((c) => (c.id === "name" ? { ...c, value: "Ada" } : c)),
      actions: [{ id: "order", label: "Place order" }],
      field_errors: [{ field: "note", message: "Too long" }],
    });
    render(<GenerativeCard cardId="h" spec={s} onSubmit={onSubmit} />);
    // gift is off, so the note is hidden: its stale error must not block.
    await user.click(screen.getByRole("button", { name: "Place order" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("removing a repeater item clears server errors keyed by item index", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "L",
      components: [
        { type: "repeater", id: "lines", value: [{ n: 1 }, { n: 2 }], fields: [{ type: "number", id: "n", label: "N" }] },
      ],
      actions: [{ id: "go", label: "Go" }],
      field_errors: [{ field: "lines[1].n", message: "Bad" }],
    });
    render(<GenerativeCard cardId="r2" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getAllByRole("button", { name: "Remove" })[1]);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values).toEqual({ lines: [{ n: 1 }] });
  });

  it("closes a confirmation whose action became hidden", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "D",
      components: [{ type: "toggle", id: "armed", label: "Armed", value: true }],
      actions: [{ id: "del", label: "Delete", style: "danger", confirm: "Really?", visible_if: "armed" }],
    });
    render(<GenerativeCard cardId="v" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Delete" }));
    expect(screen.getByText("Really?")).toBeTruthy();
    await user.click(screen.getByRole("switch"));
    expect(screen.queryByText("Really?")).toBeNull();
    expect(screen.queryByRole("button", { name: /Yes/ })).toBeNull();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("normalizes a malformed transcript submission instead of crashing", () => {
    const s = spec({
      title: "L",
      components: [{ type: "repeater", id: "lines", fields: [{ type: "number", id: "n", label: "N" }] }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(
      <GenerativeCard
        cardId="m"
        spec={s}
        submission={{ cardId: "m", actionId: "go", values: { lines: [null, "x", { n: "oops" }], junk: 1 } }}
      />,
    );
    expect(screen.getByTestId("genui-card")).toBeTruthy();
  });

  it("opens a collapsed section to show the field that blocks submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "S",
      components: [
        {
          type: "section",
          title: "More",
          collapsible: true,
          collapsed: true,
          children: [{ type: "text_input", id: "code", label: "Code", required: true }],
        },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="sec" spec={s} onSubmit={onSubmit} />);
    expect(screen.queryByLabelText(/Code/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(await screen.findByLabelText(/Code/)).toBeTruthy();
    expect(screen.getByText("Required")).toBeTruthy();
  });
});

describe("third Codex pass", () => {
  it("enforces a number input's step before submitting", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "Q",
      components: [{ type: "number", id: "n", label: "Pairs", min: 0, step: 2 }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="st" spec={s} onSubmit={onSubmit} />);
    await user.type(screen.getByLabelText("Pairs"), "3");
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Must be in steps of 2")).toBeTruthy();
  });

  it("a server error on a disabled field does not block submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "D",
      components: [{ type: "text_input", id: "acct", label: "Account", disabled: true, value: "x" }],
      actions: [{ id: "go", label: "Go" }],
      field_errors: [{ field: "acct", message: "Not found" }],
    });
    render(<GenerativeCard cardId="dis" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("a row click cannot change a table inside a disabled repeater", async () => {
    const user = userEvent.setup();
    const s = spec({
      title: "T",
      components: [
        {
          type: "repeater",
          id: "lines",
          disabled: true,
          value: [{}],
          fields: [
            { type: "table", id: "seat", label: "Seat", select: "single", row_key: "id", columns: [{ key: "id" }], rows: [{ id: "s1" }] },
          ],
        },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="tr" spec={s} onSubmit={() => {}} />);
    await user.click(screen.getByText("s1"));
    expect((screen.getByRole("radio", { name: "Select s1" }) as HTMLInputElement).checked).toBe(false);
  });

  it("quick replies hold while pending and the card locks once the reply is in the transcript", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(true);
    const s = spec({
      title: "Pick",
      components: [{ type: "text", text: "?" }],
      actions: [
        { id: "a", label: "Option A", kind: "message", message: "A please" },
        { id: "b", label: "Option B", kind: "message", message: "B please" },
      ],
    });
    const view = render(<GenerativeCard cardId="qr" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Option A" }));
    expect(onSubmit).toHaveBeenCalledWith(buildReplyMessage("qr", "a", "A please"));
    expect(await screen.findByTestId("genui-awaiting")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Option B" })).toBeNull();
    view.rerender(
      <GenerativeCard cardId="qr" spec={s} reply={{ cardId: "qr", actionId: "a", text: "A please", messageId: 9 }} onSubmit={onSubmit} />,
    );
    expect(screen.getByTestId("genui-submitted").textContent).toContain("Option A");
    expect(screen.queryByRole("button", { name: "Option B" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit and resend" })).toBeNull();
  });

  it("names radio and segmented choice groups after the field label", () => {
    const s = spec({
      title: "C",
      components: [
        { type: "choice", id: "a", label: "Channel", options: ["X", "Y"], variant: "radio" },
        { type: "choice", id: "b", label: "Format", options: ["P", "Q"] },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="ch" spec={s} onSubmit={() => {}} />);
    expect(screen.getByRole("radiogroup", { name: "Channel" })).toBeTruthy();
    expect(screen.getByRole("radiogroup", { name: "Format" })).toBeTruthy();
  });

  it("reveals a field behind a collapsed section inside an inactive tab", async () => {
    const user = userEvent.setup();
    const s = spec({
      title: "N",
      components: [
        {
          type: "tabs",
          tabs: [
            { label: "First", children: [{ type: "text", text: "hello" }] },
            {
              label: "Second",
              children: [
                {
                  type: "section",
                  title: "Advanced",
                  collapsible: true,
                  collapsed: true,
                  children: [{ type: "text_input", id: "deep", label: "Deep", required: true }],
                },
              ],
            },
          ],
        },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="nest" spec={s} onSubmit={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(await screen.findByLabelText(/Deep/)).toBeTruthy();
  });
});

describe("fourth Codex pass", () => {
  it("keeps a queued submit pending across a remount (virtualized transcript)", async () => {
    const user = userEvent.setup();
    const s = spec({ title: "P", components: [{ type: "text", text: "?" }], actions: [{ id: "a", label: "Yes", kind: "message", message: "yes" }] });
    const first = render(<GenerativeCard cardId="pend" spec={s} onSubmit={vi.fn().mockResolvedValue(true)} />);
    await user.click(screen.getByRole("button", { name: "Yes" }));
    expect(await screen.findByTestId("genui-awaiting")).toBeTruthy();
    first.unmount();
    render(<GenerativeCard cardId="pend" spec={s} onSubmit={() => {}} />);
    expect(screen.getByTestId("genui-awaiting")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Yes" })).toBeNull();
  });

  it("a required disabled field never blocks submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "D",
      components: [{ type: "text_input", id: "acct", label: "Account", required: true, disabled: true }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="rd" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("a required toggle must be switched on", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "Ack",
      components: [{ type: "toggle", id: "ok", label: "I confirm", required: true }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="tg" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    await user.click(screen.getByRole("switch"));
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("an identical edit-and-resend locks the card again", async () => {
    const user = userEvent.setup();
    const s = spec({ ...form, actions: [{ id: "order", label: "Place order" }] });
    const sub = { cardId: "re", actionId: "order", values: { name: "Ada" }, messageId: 1 };
    const view = render(<GenerativeCard cardId="re" spec={s} submission={sub} onSubmit={vi.fn().mockResolvedValue(true)} />);
    await user.click(screen.getByRole("button", { name: "Edit and resend" }));
    await user.click(screen.getByRole("button", { name: "Place order" }));
    view.rerender(
      <GenerativeCard cardId="re" spec={s} submission={{ ...sub, messageId: 2 }} onSubmit={vi.fn().mockResolvedValue(true)} />,
    );
    expect(screen.getByTestId("genui-submitted")).toBeTruthy();
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
  });
});

describe("fifth Codex pass", () => {
  it("does not re-hold a card whose message echoed before the send resolved", async () => {
    const user = userEvent.setup();
    let resolve: (v: boolean) => void = () => {};
    const onSubmit = vi.fn(() => new Promise<boolean>((r) => (resolve = r)));
    const s = spec({ ...form, actions: [{ id: "order", label: "Place order" }] });
    const view = render(<GenerativeCard cardId="echo" spec={s} onSubmit={onSubmit} />);
    await user.type(screen.getByLabelText(/Name/), "Ada");
    await user.click(screen.getByRole("button", { name: "Place order" }));
    // The direct turn echoes the user message while it is still streaming…
    view.rerender(
      <GenerativeCard cardId="echo" spec={s} submission={{ cardId: "echo", actionId: "order", values: { name: "Ada" }, messageId: 3 }} onSubmit={onSubmit} />,
    );
    // …and only then does submitPrompt resolve.
    await act(async () => resolve(true));
    expect(screen.queryByTestId("genui-awaiting")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit and resend" }));
    expect(screen.getByRole("button", { name: "Place order" })).toBeTruthy();
  });

  it("names an unlabeled input after its id", () => {
    const s = spec({ title: "U", components: [{ type: "text_input", id: "budget_note" }], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="u" spec={s} onSubmit={() => {}} />);
    expect(screen.getByLabelText("budget_note")).toBeTruthy();
  });
});

describe("sixth Codex pass", () => {
  it("a disabled required repeater never blocks submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "R",
      components: [{ type: "repeater", id: "lines", disabled: true, required: true, value: [], fields: [{ type: "number", id: "n" }] }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="dr" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("editing an item clears a server error on the repeater as a whole, not its siblings'", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "F",
      components: [
        { type: "repeater", id: "lines", min_items: 2, max_items: 2, value: [{ n: 1 }, { n: 2 }], fields: [{ type: "number", id: "n", label: "N" }] },
      ],
      actions: [{ id: "go", label: "Go" }],
      field_errors: [
        { field: "lines", message: "Totals must add to 10" },
        { field: "lines[1].n", message: "Too high" },
      ],
    });
    render(<GenerativeCard cardId="root" spec={s} onSubmit={onSubmit} />);
    const first = document.querySelector('[data-repeater-item="0"]') as HTMLElement;
    await user.type(within(first).getByLabelText("N"), "0");
    expect(screen.queryByText("Totals must add to 10")).toBeNull();
    // Item 2's own error stays until item 2 is edited.
    await user.click(screen.getByRole("button", { name: /Item 2/ }));
    expect(screen.getByText("Too high")).toBeTruthy();
  });
});

describe("tables and disabled repeaters", () => {
  it("binds a selectable table inside a repeater to its own item", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const s = spec({
      title: "Seats",
      components: [
        {
          type: "repeater",
          id: "lines",
          value: [{}, {}],
          fields: [
            {
              type: "table",
              id: "seat",
              label: "Seat",
              select: "single",
              row_key: "id",
              columns: [{ key: "id" }, { key: "name" }],
              rows: [
                { id: "s1", name: "One" },
                { id: "s2", name: "Two" },
              ],
            },
          ],
        },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="t" spec={s} onSubmit={onSubmit} />);
    // Open the second item too, then pick a different row in each.
    await user.click(screen.getByRole("button", { name: /Item 2/ }));
    const first = document.querySelector('[data-repeater-item="0"]') as HTMLElement;
    const second = document.querySelector('[data-repeater-item="1"]') as HTMLElement;
    await user.click(within(first).getByRole("radio", { name: "Select s1" }));
    await user.click(within(second).getByRole("radio", { name: "Select s2" }));
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values).toEqual({ lines: [{ seat: "s1" }, { seat: "s2" }] });
  });

  it("a disabled repeater offers no add / duplicate / remove and disables its fields", () => {
    const s = spec({
      title: "Locked lines",
      components: [
        { type: "repeater", id: "lines", disabled: true, value: [{ n: 1 }], fields: [{ type: "number", id: "n", label: "N" }] },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="d" spec={s} onSubmit={() => {}} />);
    expect(screen.queryByRole("button", { name: "+ Add" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Duplicate" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.getByLabelText("N")).toBeDisabled();
    // Reading stays possible: the item still collapses.
    expect(screen.getByRole("button", { name: /Item 1/ })).toBeEnabled();
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

describe("seventh Codex pass", () => {
  it("opens a collapsed section inside a repeater item to reveal that item's field", async () => {
    const user = userEvent.setup();
    const s = spec({
      title: "R",
      components: [
        {
          type: "repeater",
          id: "lines",
          value: [{ cpm: 1 }, {}],
          fields: [
            {
              type: "section",
              title: "Pricing",
              collapsible: true,
              collapsed: true,
              children: [{ type: "number", id: "cpm", label: "CPM", required: true }],
            },
          ],
        },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    const onSubmit = vi.fn();
    render(<GenerativeCard cardId="rsec" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    // Item 2 (index 1) opened, and its Pricing section with it; item 1's stays shut.
    const item = document.querySelector("[data-repeater-item='1']") as HTMLElement;
    expect(await within(item).findByLabelText(/CPM/)).toBeTruthy();
    const first = document.querySelector("[data-repeater-item='0']") as HTMLElement;
    expect(within(first).queryByLabelText(/CPM/)).toBeNull();
  });

  it("drops a hold whose message echoed while the card was unmounted", async () => {
    const user = userEvent.setup();
    const s = spec({ title: "H", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    const first = render(<GenerativeCard cardId="hold" spec={s} onSubmit={vi.fn().mockResolvedValue(true)} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(await screen.findByTestId("genui-awaiting")).toBeTruthy();
    first.unmount();
    // Remounts with the echo already in the transcript.
    render(
      <GenerativeCard
        cardId="hold"
        spec={s}
        submission={{ cardId: "hold", actionId: "go", values: { n: "" }, messageId: 9 }}
        onSubmit={() => {}}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Edit and resend" }));
    expect(screen.queryByTestId("genui-awaiting")).toBeNull();
    expect(screen.getByRole("button", { name: "Go" })).toBeTruthy();
    expect(window.localStorage.getItem("fleet.genui.pending.hold")).toBeNull();
  });
});

describe("cards from before the summary", () => {
  it("render locked with no actions and say to ask again", () => {
    const s = spec({ title: "Old", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="old" spec={s} retired onSubmit={() => {}} />);
    expect(screen.getByTestId("genui-retired")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Go" })).toBeNull();
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
  });

  it("keep showing an answer they already had, without Edit and resend", () => {
    const s = spec({ title: "Old", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    render(
      <GenerativeCard cardId="old2" spec={s} retired submission={{ cardId: "old2", actionId: "go", values: { n: "A" } }} onSubmit={() => {}} />,
    );
    expect(screen.getByTestId("genui-submitted")).toBeTruthy();
    expect(screen.queryByTestId("genui-retired")).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit and resend" })).toBeNull();
  });
});

describe("submission size", () => {
  it("refuses, before sending, an answer larger than the chat accepts", async () => {
    const user = userEvent.setup();
    const long = Array.from({ length: 20000 }, (_, i) => `publisher-${String(i).padStart(6, "0")}.a-rather-long-example-domain-name.example.com`);
    const s = spec({
      title: "Big",
      components: [{ type: "list_input", id: "domains", label: "Domains", value: long }],
      actions: [{ id: "go", label: "Go" }],
    });
    const onSubmit = vi.fn().mockResolvedValue(true);
    render(<GenerativeCard cardId="big" spec={s} onSubmit={onSubmit} />);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByRole("status").textContent).toMatch(/Too large to send/);
  });
});

describe("eighth Codex pass", () => {
  it("names an unlabeled toggle by its id", () => {
    const s = spec({
      title: "T",
      components: [
        { type: "toggle", id: "accept_terms" },
        { type: "toggle", id: "enable_alerts" },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="tog" spec={s} onSubmit={() => {}} />);
    expect(screen.getByRole("switch", { name: "accept_terms" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "enable_alerts" })).toBeTruthy();
  });

  it("shows a 0 that replaces a blank number field", async () => {
    const user = userEvent.setup();
    const s = spec({
      title: "Z",
      components: [{ type: "repeater", id: "lines", value: [{}, { n: 0 }], fields: [{ type: "number", id: "n", label: "N" }] }],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="zero" spec={s} onSubmit={() => {}} />);
    // Open the second item, then remove the first: item 0's input now shows item 1's 0.
    const toggles = document.querySelectorAll("[data-repeater-item] button[aria-expanded]");
    await user.click(toggles[1] as HTMLElement);
    await user.click(screen.getAllByRole("button", { name: "Remove" })[0]);
    const inputs = screen.getAllByLabelText(/^N/) as HTMLInputElement[];
    expect(inputs.map((i) => i.value)).toContain("0");
    expect(inputs.every((i) => i.value === "0")).toBe(true);
  });
});

describe("ninth Codex pass", () => {
  it("keeps the progressbar's ARIA value inside its range", () => {
    const s = spec({
      title: "P",
      components: [
        { type: "number", id: "q", label: "Q", value: 250 },
        { type: "progress", label: "Used", value: "q", max: 100 },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    render(<GenerativeCard cardId="prog" spec={s} onSubmit={() => {}} />);
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("100");
    expect(screen.getByText(/250 \/ 100/)).toBeTruthy();
  });

  it("keeps a bounded number of drafts, dropping the least recently edited", async () => {
    window.localStorage.clear();
    for (let i = 0; i < 25; i++) {
      window.localStorage.setItem(`fleet.genui.draft.old${i}`, JSON.stringify({ values: { n: "x" }, at: Date.now() - 1000 + i }));
    }
    window.localStorage.setItem("fleet.genui.draft.legacy", JSON.stringify({ n: "no timestamp" }));
    window.localStorage.setItem("fleet.genui.draft.stale", JSON.stringify({ values: { n: "x" }, at: Date.now() - 30 * 24 * 3600 * 1000 }));
    const user = userEvent.setup();
    const s = spec({ title: "D", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="fresh" spec={s} onSubmit={() => {}} />);
    await user.type(screen.getByLabelText(/Name/), "A");
    const keys = Object.keys(window.localStorage).filter((k) => k.startsWith("fleet.genui.draft."));
    expect(keys.length).toBe(20);
    expect(keys).toContain("fleet.genui.draft.fresh");
    expect(keys).toContain("fleet.genui.draft.old24");
    expect(keys).not.toContain("fleet.genui.draft.old0");
    expect(keys).not.toContain("fleet.genui.draft.legacy");
    expect(keys).not.toContain("fleet.genui.draft.stale");
  });
});

describe("tenth Codex pass", () => {
  it("lets an optional radio choice be cleared, but not a required one", async () => {
    const user = userEvent.setup();
    const s = spec({
      title: "R",
      components: [
        { type: "choice", id: "opt", label: "Optional", variant: "radio", options: ["a", "b"] },
        { type: "choice", id: "req", label: "Required", variant: "radio", options: ["a", "b"], required: true, value: "a" },
      ],
      actions: [{ id: "go", label: "Go" }],
    });
    const onSubmit = vi.fn().mockResolvedValue(true);
    render(<GenerativeCard cardId="radio" spec={s} onSubmit={onSubmit} />);
    expect(screen.queryAllByRole("button", { name: "Clear choice" })).toHaveLength(0);
    const opt = screen.getByRole("radiogroup", { name: "Optional" });
    await user.click(within(opt).getAllByRole("radio")[1]);
    await user.click(screen.getByRole("button", { name: "Clear choice" }));
    expect(within(opt).getAllByRole("radio").every((r) => !(r as HTMLInputElement).checked)).toBe(true);
    await user.click(screen.getByRole("button", { name: "Go" }));
    expect(parseSubmissionMessage(onSubmit.mock.calls[0][0])?.values).toMatchObject({ opt: "", req: "a" });
  });

  it("draws a chart whose values span nearly the whole float range", () => {
    const s = spec({
      title: "C",
      components: [{ type: "chart", kind: "line", labels: ["a", "b"], series: [{ name: "s", values: [-1e308, 1e308] }] }],
    });
    render(<GenerativeCard cardId="huge" spec={s} />);
    const svg = document.querySelector("svg") as SVGElement;
    expect(svg.innerHTML).not.toContain("NaN");
    expect(svg.querySelectorAll("circle")).toHaveLength(2);
  });

  it("restores an unsent Edit-and-resend draft, in edit mode, after a remount", async () => {
    window.localStorage.clear();
    const user = userEvent.setup();
    const s = spec({ title: "E", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    const submission = { cardId: "edit", actionId: "go", values: { n: "Ada" }, messageId: 4 };
    const first = render(<GenerativeCard cardId="edit" spec={s} submission={submission} onSubmit={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Edit and resend" }));
    await user.clear(screen.getByLabelText(/Name/));
    await user.type(screen.getByLabelText(/Name/), "Grace");
    first.unmount();
    render(<GenerativeCard cardId="edit" spec={s} submission={submission} onSubmit={() => {}} />);
    expect((screen.getByLabelText(/Name/) as HTMLInputElement).value).toBe("Grace");
    expect(screen.getByLabelText(/Name/)).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Go" })).toBeTruthy();
  });

  it("drops a pre-answer draft once that answer reached the transcript while unmounted", () => {
    window.localStorage.setItem("fleet.genui.draft.done", JSON.stringify({ values: { n: "typed" }, at: Date.now(), after: "" }));
    const s = spec({ title: "E", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="done" spec={s} submission={{ cardId: "done", actionId: "go", values: { n: "sent" } }} onSubmit={() => {}} />);
    expect((screen.getByLabelText(/Name/) as HTMLInputElement).value).toBe("sent");
    expect(screen.getByLabelText(/Name/)).toBeDisabled();
    expect(window.localStorage.getItem("fleet.genui.draft.done")).toBeNull();
  });

  it("makes room for a draft by evicting older drafts when storage is full", async () => {
    window.localStorage.clear();
    window.localStorage.setItem("fleet.genui.draft.older", JSON.stringify({ values: { n: "x" }, at: Date.now() - 1000, after: "" }));
    const realSet = Storage.prototype.setItem;
    const spy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(function (this: Storage, k: string, v: string) {
      // Full while the older draft is still there.
      if (k === "fleet.genui.draft.big" && this.getItem("fleet.genui.draft.older") !== null) {
        throw new DOMException("full", "QuotaExceededError");
      }
      realSet.call(this, k, v);
    });
    try {
      const user = userEvent.setup();
      const s = spec({ title: "B", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
      render(<GenerativeCard cardId="big" spec={s} onSubmit={() => {}} />);
      await user.type(screen.getByLabelText(/Name/), "A");
    } finally {
      spy.mockRestore();
    }
    expect(window.localStorage.getItem("fleet.genui.draft.older")).toBeNull();
    expect(window.localStorage.getItem("fleet.genui.draft.big")).toContain('"A"');
  });
});

describe("eleventh Codex pass", () => {
  it("names a selectable table's group by its field label", () => {
    const table = (id: string, label: string) => ({
      type: "table",
      id,
      label,
      select: "single",
      row_key: "id",
      columns: [{ key: "id" }],
      rows: [{ id: "acct_1" }],
    });
    const s = spec({ title: "T", components: [table("from", "Move from"), table("to", "Move to")], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="tbl" spec={s} onSubmit={() => {}} />);
    expect(screen.getByRole("group", { name: "Move from" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Move to" })).toBeTruthy();
  });

  it("keeps a server error the user answered cleared across a remount", async () => {
    window.localStorage.clear();
    const user = userEvent.setup();
    const s = spec({
      title: "F",
      components: [{ type: "text_input", id: "n", label: "Name", value: "bad" }],
      actions: [{ id: "go", label: "Go" }],
      field_errors: [{ field: "n", message: "Not allowed" }],
    });
    const first = render(<GenerativeCard cardId="fe" spec={s} onSubmit={() => {}} />);
    expect(screen.getByText("Not allowed")).toBeTruthy();
    await user.clear(screen.getByLabelText(/Name/));
    await user.type(screen.getByLabelText(/Name/), "good");
    expect(screen.queryByText("Not allowed")).toBeNull();
    first.unmount();
    render(<GenerativeCard cardId="fe" spec={s} onSubmit={() => {}} />);
    expect((screen.getByLabelText(/Name/) as HTMLInputElement).value).toBe("good");
    expect(screen.queryByText("Not allowed")).toBeNull();
  });

  it("does not scan other drafts on an ordinary edit", async () => {
    window.localStorage.clear();
    const user = userEvent.setup();
    const s = spec({ title: "S", components: [{ type: "text_input", id: "n", label: "Name" }], actions: [{ id: "go", label: "Go" }] });
    render(<GenerativeCard cardId="scan" spec={s} onSubmit={() => {}} />);
    await user.type(screen.getByLabelText(/Name/), "a");
    const keySpy = vi.spyOn(Storage.prototype, "key");
    try {
      await user.type(screen.getByLabelText(/Name/), "bcd");
      expect(keySpy).not.toHaveBeenCalled();
    } finally {
      keySpy.mockRestore();
    }
  });
});
