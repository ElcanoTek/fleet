import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { NativeSelect, NativeSelectOption } from "./native-select";

describe("NativeSelect", () => {
  it("defaults to full width with right chevron padding", () => {
    render(
      <NativeSelect aria-label="Default select" defaultValue="a">
        <NativeSelectOption value="a">Alpha</NativeSelectOption>
      </NativeSelect>,
    );

    const select = screen.getByRole("combobox", { name: "Default select" });
    const wrapper = select.closest('[data-slot="native-select-wrapper"]');
    expect(select).toHaveClass("w-full", "pl-3", "pr-8", "data-[size=sm]:pr-7");
    expect(wrapper).toHaveClass("w-full", "has-[>select:not(.w-full)]:w-fit");
  });

  it("drops w-full when w-auto is passed so inline toolbars shrink-wrap", () => {
    render(
      <NativeSelect
        aria-label="Toolbar select"
        size="sm"
        className="filter-select w-auto"
        defaultValue="7"
      >
        <NativeSelectOption value="7">Last 7 days</NativeSelectOption>
      </NativeSelect>,
    );

    const select = screen.getByRole("combobox", { name: "Toolbar select" });
    expect(select).toHaveClass("filter-select", "w-auto", "pr-8", "data-[size=sm]:pr-7");
    expect(select).not.toHaveClass("w-full");
    expect(select).toHaveAttribute("data-size", "sm");
  });

  it("retains right chevron gutter when callers pass shorthand px-* or p-* classes", () => {
    render(
      <NativeSelect
        aria-label="Dense select"
        className="h-9 bg-[var(--color-bg)] px-2.5 py-2 text-[0.85rem]"
        defaultValue="minutes"
      >
        <NativeSelectOption value="minutes">Minutes</NativeSelectOption>
      </NativeSelect>,
    );

    const select = screen.getByRole("combobox", { name: "Dense select" });
    expect(select).toHaveClass("pr-8", "data-[size=sm]:pr-7");
  });

  it("allows callers to override right padding explicitly with pr-* or pe-*", () => {
    render(
      <NativeSelect aria-label="Custom right gutter" className="pr-10" defaultValue="a">
        <NativeSelectOption value="a">Alpha</NativeSelectOption>
      </NativeSelect>,
    );

    const select = screen.getByRole("combobox", { name: "Custom right gutter" });
    expect(select).toHaveClass("pr-10");
    expect(select).not.toHaveClass("pr-8");
  });
});
