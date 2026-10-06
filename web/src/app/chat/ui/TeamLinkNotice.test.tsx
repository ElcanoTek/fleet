import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { TeamLinkNotice } from "./TeamLinkNotice";

// The two dead ends a team link can land on. What they leave out is the
// point: no title, transcript, files — and for B22 no project name.

afterEach(cleanup);

describe("TeamLinkNotice — signed in, not on the team (B22)", () => {
  it("names the team and the signed-in account, and nothing about the chat", () => {
    const { container } = render(
      <TeamLinkNotice
        notice={{ kind: "not_on_team", team: "Elcano", viewerEmail: "dana@clientco.com" }}
        onOpenProject={() => {}}
      />,
    );
    expect(
      screen.getByRole("heading", { name: "This chat is shared with Elcano" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Only Elcano members can open it. If you think you should have access, ask the person who sent you the link.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("Signed in as dana@clientco.com")).toBeInTheDocument();
    // A dead end: no way into the chat or its project from here.
    expect(container.querySelector("button, a")).toBeNull();
  });
});

describe("TeamLinkNotice — not shared anymore (B23)", () => {
  it("offers the project only when the viewer can still see it", () => {
    const onOpenProject = vi.fn();
    render(
      <TeamLinkNotice
        notice={{ kind: "not_shared", project: { id: "p1", name: "Knowertech" } }}
        onOpenProject={onOpenProject}
      />,
    );
    expect(
      screen.getByRole("heading", { name: "This chat isn’t shared anymore" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Ask the person who sent the link if you still need it."),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Open Knowertech" }));
    expect(onOpenProject).toHaveBeenCalledWith("p1");
  });

  it("offers nothing without a visible project", () => {
    const { container } = render(
      <TeamLinkNotice notice={{ kind: "not_shared" }} onOpenProject={() => {}} />,
    );
    expect(container.querySelector("button")).toBeNull();
  });
});
