import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import GenerativeCard from "./GenerativeCard";
import type { CardSpec } from "./model";

const spec: CardSpec = {
  title: "Follow up",
  components: [{ type: "text_input", id: "note", label: "Note" }],
  actions: [{ id: "go", label: "Go" }],
};

afterEach(() => vi.restoreAllMocks());

describe("card drafts without storage", () => {
  it("survive a remount (dashboard ↔ normal view) when localStorage is blocked", () => {
    for (const m of ["getItem", "setItem", "removeItem", "key"] as const) {
      vi.spyOn(Storage.prototype, m).mockImplementation(() => {
        throw new Error("blocked");
      });
    }
    const first = render(<GenerativeCard cardId="c-mem" spec={spec} storageScope="conv" onSubmit={vi.fn()} />);
    fireEvent.change(screen.getByLabelText(/Note/), { target: { value: "half typed" } });
    first.unmount();
    render(<GenerativeCard cardId="c-mem" spec={spec} storageScope="conv" variant="panel" onSubmit={vi.fn()} />);
    expect(screen.getByLabelText(/Note/)).toHaveValue("half typed");
  });
});
