import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import NoAccessPage from "./page";

// The dead-end for a signed-in non-member: neutral copy (no Elcano wording
// on a white-labelled deployment), a retry (an administrator may have just
// added them), and the same sign-out every surface uses, which also ends a
// central-Auth session.
describe("/no-access", () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("offers a retry and a top-level sign-out form", () => {
    render(<NoAccessPage />);
    expect(
      screen.getByRole("heading", { name: /no access yet/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/hasn.t been added to this workspace yet/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Elcano/)).toBeNull();
    expect(screen.getByRole("link", { name: /try again/i })).toHaveAttribute(
      "href",
      "/chat",
    );
    // Still a real form, so signing out works before the page has hydrated.
    const button = screen.getByRole("button", { name: /sign out/i });
    const form = button.closest("form")!;
    expect(form.getAttribute("method")).toBe("post");
    expect(form.getAttribute("action")).toBe("/api/auth/logout");
  });

  it("lets go of this browser's push subscription before the logout form posts", async () => {
    // A non-member cannot turn notifications on, so a subscription this
    // browser holds was made by an account whose session ended without a
    // sign-out: the next person to use the browser must not get its alerts.
    // The server refuses a non-member's delete; the browser's own unsubscribe
    // is what stops delivery, and the relay's 404/410 then retires the row.
    const events: string[] = [];
    Object.defineProperty(navigator, "serviceWorker", {
      configurable: true,
      value: {
        getRegistration: async () => ({
          pushManager: {
            getSubscription: async () => ({
              endpoint: "https://relay.example/ep",
              unsubscribe: async () => {
                events.push("browser unsubscribe");
                return true;
              },
            }),
          },
        }),
      },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit) => {
        events.push(`${init.method} ${url}`);
        return Response.json({ error: "not_a_member" }, { status: 403 });
      }),
    );
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(
      function (this: HTMLFormElement) {
        events.push(`${this.method} ${new URL(this.action).pathname}`);
      },
    );
    render(<NoAccessPage />);
    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));
    await waitFor(() => expect(events).toContain("post /api/auth/logout"));
    expect(events).toEqual([
      "DELETE /api/push/unsubscribe",
      "browser unsubscribe",
      "post /api/auth/logout",
    ]);
  });
});
