import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { FormActions, MissingFieldsHint, useFocusOnOpen } from "./atoms";

afterEach(cleanup);

function Form({ open, item }: { open: boolean; item: string | null }) {
  const ref = useFocusOnOpen<HTMLInputElement>(open, item);
  return (
    <>
      <button type="button">Opener</button>
      {open ? <input ref={ref} aria-label="First field" /> : null}
    </>
  );
}

describe("useFocusOnOpen", () => {
  it("puts the cursor in the first field when the form opens", () => {
    const { rerender } = render(<Form open={false} item={null} />);
    screen.getByRole("button", { name: "Opener" }).focus();
    rerender(<Form open item="" />);
    expect(screen.getByLabelText("First field")).toHaveFocus();
  });

  it("leaves a form that is already open on first render alone", () => {
    render(<Form open item="a" />);
    expect(screen.getByLabelText("First field")).not.toHaveFocus();
  });

  it("moves the cursor again when the open form is given another item, and only then", () => {
    const { rerender } = render(<Form open={false} item={null} />);
    rerender(<Form open item="a" />);
    const opener = screen.getByRole("button", { name: "Opener" });
    // Typing re-renders with the same item: focus that has moved on stays put.
    opener.focus();
    rerender(<Form open item="a" />);
    expect(opener).toHaveFocus();
    // Edit on another row: the form's contents swap, the cursor follows.
    rerender(<Form open item="b" />);
    expect(screen.getByLabelText("First field")).toHaveFocus();
  });
});

describe("MissingFieldsHint", () => {
  it("names the missing fields and renders nothing when none are missing", () => {
    const { rerender, container } = render(<MissingFieldsHint id="m" missing={["Name", "Server URL"]} />);
    expect(container.querySelector("#m")).toHaveTextContent("To continue, fill in: Name, Server URL");
    rerender(<MissingFieldsHint id="m" missing={[]} />);
    expect(container.querySelector("#m")).toBeNull();
  });

  it("is a line of its own in the block form, under the button it explains", () => {
    const { container } = render(<MissingFieldsHint block align="left" id="m" missing={["Name"]} />);
    const hint = container.querySelector("#m");
    expect(hint?.tagName).toBe("P");
    expect(hint).toHaveClass("basis-full", "text-left");
  });
});

describe("FormActions", () => {
  // The structure is the fix: the buttons are one flex item, so the hint can
  // never take one of them onto its line.
  it("keeps the buttons in one group, separate from the hint", () => {
    render(
      <FormActions hint={<MissingFieldsHint id="m" missing={["Name"]} />}>
        <button type="button">Cancel</button>
        <button type="button">Create</button>
      </FormActions>,
    );
    const cancel = screen.getByRole("button", { name: "Cancel" });
    const create = screen.getByRole("button", { name: "Create" });
    const group = cancel.parentElement;
    expect(create.parentElement).toBe(group);
    const footer = group?.parentElement;
    expect(Array.from(footer?.children ?? [])).toEqual([document.getElementById("m"), group]);
  });
});
