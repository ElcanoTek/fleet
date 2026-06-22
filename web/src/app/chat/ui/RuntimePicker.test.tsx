import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { RuntimePicker, type RuntimeFlavor } from "./RuntimePicker";

const FLAVORS: RuntimeFlavor[] = [
  {
    name: "native-inprocess",
    display_name: "Native (in-process)",
    description: "Fast in-process loop.",
  },
  {
    name: "native-acp",
    display_name: "Native (sandboxed)",
    description: "Sandboxed ACP agent. Fully governed.",
  },
];

describe("RuntimePicker", () => {
  it("hides when fewer than two flavors are available", () => {
    const { container } = render(
      <RuntimePicker
        flavors={[FLAVORS[0]]}
        selected=""
        defaultRuntime="native-inprocess"
        onSelect={() => {}}
      />,
    );
    expect(container.querySelector('[data-testid="runtime-picker"]')).toBeNull();
  });

  it("falls back to the default flavor label when none is selected", () => {
    render(
      <RuntimePicker
        flavors={FLAVORS}
        selected=""
        defaultRuntime="native-inprocess"
        onSelect={() => {}}
      />,
    );
    expect(screen.getByTestId("runtime-picker-button")).toHaveTextContent("Native (in-process)");
  });

  it("shows the selected flavor label over the default", () => {
    render(
      <RuntimePicker
        flavors={FLAVORS}
        selected="native-acp"
        defaultRuntime="native-inprocess"
        onSelect={() => {}}
      />,
    );
    expect(screen.getByTestId("runtime-picker-button")).toHaveTextContent("Native (sandboxed)");
  });

  it("opens the menu and reports the chosen flavor", () => {
    const onSelect = vi.fn();
    render(
      <RuntimePicker
        flavors={FLAVORS}
        selected=""
        defaultRuntime="native-inprocess"
        onSelect={onSelect}
      />,
    );
    fireEvent.click(screen.getByTestId("runtime-picker-button"));
    expect(screen.getByTestId("runtime-picker-menu")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("runtime-option-native-acp"));
    expect(onSelect).toHaveBeenCalledWith("native-acp");
  });
});
