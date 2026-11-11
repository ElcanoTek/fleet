import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { rich } from "./rich";

vi.mock("./index", () => ({
  t: (source: string) => source === "Owner {0}, link {1}" ? "Link {1}; owner {0}" : source,
}));

afterEach(cleanup);

it("reorders rich slots without turning user text into markup or another slot", () => {
  const userText = "<img src=x onerror=alert(1)>{1}";
  const { container } = render(<p>{rich("Owner {0}, link {1}", userText, <a href="/settings">Settings</a>)}</p>);
  expect(container.textContent).toBe(`Link Settings; owner ${userText}`);
  expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");
  expect(container.querySelector("img")).toBeNull();
});
