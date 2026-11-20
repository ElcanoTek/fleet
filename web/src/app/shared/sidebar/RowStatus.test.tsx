import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RowStatus, compactUntil, describeRowStatus, type RowStatusInput } from "./RowStatus";

afterEach(() => cleanup());

const NOW = 1_800_000_000;

function shown(status: RowStatusInput): string | undefined {
  cleanup();
  render(<RowStatus status={status} now={NOW} />);
  return screen.queryByTestId("row-status")?.getAttribute("data-status") ?? undefined;
}

describe("compactUntil", () => {
  it("says how long to one unit", () => {
    expect(compactUntil(NOW + 30, NOW)).toBe("now");
    expect(compactUntil(NOW - 500, NOW)).toBe("now");
    expect(compactUntil(NOW + 45 * 60, NOW)).toBe("45m");
    expect(compactUntil(NOW + 19 * 3600, NOW)).toBe("19h");
    expect(compactUntil(NOW + 2 * 86400, NOW)).toBe("2d");
    // Never "0m" or "0h": a minute and a half is a minute or two, not none.
    expect(compactUntil(NOW + 61, NOW)).toBe("1m");
  });
});

describe("RowStatus", () => {
  it("draws nothing for a plain chat", () => {
    render(<RowStatus status={{}} now={NOW} />);
    expect(screen.queryByTestId("row-status")).toBeNull();
  });

  it("shows one status: the first that applies, in the order of what matters most", () => {
    const everything: RowStatusInput = {
      streaming: true,
      expiresIn: 3,
      patrol: { status: "active", nextRunAt: NOW + 3600 },
      live: true,
      teamShared: true,
      linkShared: true,
      sealed: true,
    };
    const order = ["working", "expiring", "patrol", "live", "team", "link", "sealed"];
    const drop: (keyof RowStatusInput)[] = ["streaming", "expiresIn", "patrol", "live", "teamShared", "linkShared", "sealed"];
    const status = { ...everything };
    for (let i = 0; i < order.length; i++) {
      expect(shown(status)).toBe(order[i]);
      delete status[drop[i]];
    }
    expect(shown(status)).toBeUndefined();
  });

  it("a patrol says when it checks next, or that it is paused", () => {
    render(<RowStatus status={{ patrol: { status: "active", nextRunAt: NOW + 19 * 3600 } }} now={NOW} />);
    expect(screen.getByTestId("row-status")).toHaveTextContent("19h");
    cleanup();
    render(<RowStatus status={{ patrol: { status: "paused", nextRunAt: NOW + 3600 } }} now={NOW} />);
    expect(screen.getByTestId("row-status")).toHaveTextContent("Paused");
    expect(screen.getByTestId("row-status").getAttribute("data-status")).toBe("patrol-paused");
  });

  it("a temporary chat in the warning window says its days left", () => {
    render(<RowStatus status={{ expiresIn: 6 }} now={NOW} />);
    expect(screen.getByTestId("row-status")).toHaveTextContent("6d");
    cleanup();
    // Inside the last day it is still "1d", never "0d".
    render(<RowStatus status={{ expiresIn: 0 }} now={NOW} />);
    expect(screen.getByTestId("row-status")).toHaveTextContent("1d");
  });

  it("a teammate's chat carries its owner's initial beside the status, and alone when there is none", () => {
    render(<RowStatus status={{ live: true, owner: "jeanne@example.com" }} now={NOW} />);
    expect(screen.getByTestId("row-status").getAttribute("data-status")).toBe("live");
    expect(screen.getByTestId("row-owner")).toHaveTextContent("J");
    cleanup();
    render(<RowStatus status={{ owner: "connor@example.com" }} now={NOW} />);
    expect(screen.getByTestId("row-status").getAttribute("data-status")).toBeNull();
    expect(screen.getByTestId("row-owner")).toHaveTextContent("C");
  });

  it("is decoration: hidden from a screen reader, which gets the sentence instead", () => {
    render(<RowStatus status={{ teamShared: true, owner: "jeanne@example.com" }} now={NOW} />);
    expect(screen.getByTestId("row-status").getAttribute("aria-hidden")).toBe("true");
    expect(describeRowStatus({ teamShared: true, owner: "jeanne@example.com" }, NOW)).toBe(
      "Shared with your team — teammates can read it · Shared by jeanne@example.com",
    );
    expect(describeRowStatus({ patrol: { status: "active", nextRunAt: NOW + 7200 } }, NOW)).toBe("Patrol — next check in 2h");
    expect(describeRowStatus({ patrol: { status: "active" } }, NOW)).toBe("Patrol — this chat checks in on a schedule");
    expect(describeRowStatus({}, NOW)).toBe("");
  });
});
