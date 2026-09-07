import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import NotificationsAdminPage from "./page";

// Settings → Admin → Notifications — admin-managed task notifications.
// Load-bearing assertions: the env view renders with honest channel status
// and no secret material, a save sends typed secrets (and only typed
// secrets), untouched secret fields are omitted (keep), clear sends "",
// revert DELETEs, and the test button reports the key-free result.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; force "admin" so the page renders. (The real
// hook probes an admin endpoint; authorization stays server-side regardless.)
vi.mock("../../useIsAdmin", () => ({
  useIsAdmin: () => "admin",
}));

type View = {
  source: "admin" | "env";
  settings: Record<string, unknown>;
  email_enabled: boolean;
  webhook_enabled: boolean;
};

const ENV_VIEW: View = {
  source: "env",
  settings: {
    notify_on: "",
    smtp_host: "smtp.env.example",
    smtp_port: "587",
    smtp_username: "",
    has_smtp_password: true,
    smtp_from: "fleet@env.example",
    email_to: "ops@env.example",
    webhook_url: "",
    webhook_method: "POST",
    webhook_body_template: "",
    has_webhook_secret: false,
  },
  email_enabled: true,
  webhook_enabled: false,
};

function mockFetch(
  view: View,
  onWrite?: (url: string, init: RequestInit) => { status: number; body: unknown } | undefined,
) {
  let current = view;
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    if (!init || init.method === undefined || init.method === "GET") {
      return { ok: true, status: 200, json: async () => current };
    }
    const custom = onWrite?.(url, init);
    if (custom) {
      if (custom.status < 400 && (custom.body as View).source) {
        current = custom.body as View;
      }
      return {
        ok: custom.status < 400,
        status: custom.status,
        json: async () => custom.body,
        text: async () => JSON.stringify(custom.body),
      };
    }
    return { ok: true, status: 200, json: async () => current, text: async () => "{}" };
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("NotificationsAdminPage", () => {
  it("renders the env view with honest channel status and no secrets", async () => {
    vi.stubGlobal("fetch", mockFetch(ENV_VIEW));
    render(<NotificationsAdminPage />);
    expect(await screen.findByText("Env config", undefined, { timeout: 5000 })).toBeInTheDocument();
    expect(screen.getByTestId("notify-smtp-host")).toHaveValue("smtp.env.example");
    // Stored password shows status only, never a value.
    expect(screen.getByText(/stored — leave blank to keep/)).toBeInTheDocument();
    expect(screen.getByTestId("notify-smtp-password")).toHaveValue("");
    expect(screen.getByText("Configured")).toBeInTheDocument(); // email channel
    expect(screen.getByText("Not configured")).toBeInTheDocument(); // webhook channel
    // No revert button for env config.
    expect(screen.queryByTestId("notify-revert")).toBeNull();
  });

  it("saves with write-only secret semantics (typed = sent, untouched = omitted)", async () => {
    const fetchMock = mockFetch(ENV_VIEW, (url, init) => {
      if (init.method === "PUT") {
        return {
          status: 200,
          body: {
            ...ENV_VIEW,
            source: "admin",
            settings: { ...ENV_VIEW.settings, updated_by: "boss@x.com" },
          },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);

    // Type a webhook URL + its secret; leave the SMTP password untouched.
    fireEvent.change(await screen.findByTestId("notify-webhook-url", undefined, { timeout: 5000 }), {
      target: { value: "https://hooks.example.com/x" },
    });
    fireEvent.change(screen.getByTestId("notify-webhook-secret"), {
      target: { value: "new-hook-secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: "failure" }));
    fireEvent.click(screen.getByTestId("notify-save"));

    await waitFor(() => expect(screen.getByText("Overridden")).toBeInTheDocument());
    expect(screen.getByText(/Last saved by boss@x.com/)).toBeInTheDocument();
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT");
    const body = JSON.parse(String(put?.[1]?.body));
    expect(body.webhook_url).toBe("https://hooks.example.com/x");
    expect(body.webhook_secret).toBe("new-hook-secret");
    expect(body.notify_on).toBe("failure");
    // Untouched stored SMTP password: field omitted entirely (keep).
    expect("smtp_password" in body).toBe(false);
    expect(screen.getByTestId("notify-revert")).toBeInTheDocument();
  });

  it("clearing a stored secret sends an explicit empty string", async () => {
    const fetchMock = mockFetch(ENV_VIEW, (url, init) => {
      if (init.method === "PUT") return { status: 200, body: ENV_VIEW };
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);
    fireEvent.click(await screen.findByTestId("notify-smtp-password-clear", undefined, { timeout: 5000 }));
    fireEvent.click(screen.getByTestId("notify-save"));
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === "PUT")).toBe(true),
    );
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body)).smtp_password).toBe("");
  });

  it("reverting to env config DELETEs only after the inline confirm's second click", async () => {
    const adminView: View = { ...ENV_VIEW, source: "admin" };
    const fetchMock = mockFetch(adminView, (url, init) => {
      if (init.method === "DELETE") return { status: 200, body: ENV_VIEW };
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    // No native dialog: a window.confirm call would be a regression.
    const confirmSpy = vi.fn().mockReturnValue(true);
    vi.stubGlobal("confirm", confirmSpy);
    render(<NotificationsAdminPage />);
    const revert = await screen.findByTestId("notify-revert", undefined, { timeout: 5000 });
    const deletes = () => fetchMock.mock.calls.filter(([, i]) => i?.method === "DELETE");
    fireEvent.click(revert); // arms
    expect(revert).toHaveTextContent("Confirm: discard saved settings");
    expect(deletes()).toHaveLength(0);
    fireEvent.click(revert); // fires
    await waitFor(() => expect(screen.getByText("Env config")).toBeInTheDocument());
    expect(deletes()).toHaveLength(1);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it("disables Send test while the form is dirty and says why, re-enabling after save", async () => {
    const fetchMock = mockFetch(ENV_VIEW, (url, init) => {
      if (init.method === "PUT") return { status: 200, body: { ...ENV_VIEW, source: "admin" } };
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);
    const testBtn = await screen.findByTestId("notify-test-email", undefined, { timeout: 5000 });
    expect(testBtn).toBeEnabled();
    expect(testBtn).not.toHaveAttribute("title");
    fireEvent.change(screen.getByTestId("notify-webhook-url"), {
      target: { value: "https://hooks.example.com/x" },
    });
    // Dirty: a test now would silently exercise the OLD saved config.
    expect(testBtn).toBeDisabled();
    expect(testBtn).toHaveAttribute("title", "Tests use the saved config; save your changes first.");
    expect(screen.getByTestId("notify-test-webhook")).toBeDisabled();
    fireEvent.click(testBtn);
    expect(fetchMock.mock.calls.some(([u]) => String(u).includes("/test"))).toBe(false);
    fireEvent.click(screen.getByTestId("notify-save"));
    await waitFor(() => expect(screen.getByTestId("notify-test-email")).toBeEnabled());
  });

  it("the notify-on chips parse and rewrite the CSV, preserving unknown tokens", async () => {
    const view: View = {
      ...ENV_VIEW,
      settings: { ...ENV_VIEW.settings, notify_on: "failure, custom_status" },
    };
    const fetchMock = mockFetch(view, (url, init) => {
      if (init.method === "PUT") return { status: 200, body: view };
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);
    const failure = await screen.findByRole("button", { name: "failure" }, { timeout: 5000 });
    expect(failure).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "success" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    // Toggle failure off, success on: custom_status must survive the rewrite.
    fireEvent.click(failure);
    fireEvent.click(screen.getByRole("button", { name: "success" }));
    fireEvent.click(screen.getByTestId("notify-save"));
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === "PUT")).toBe(true),
    );
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT");
    const sent = String(JSON.parse(String(put?.[1]?.body)).notify_on)
      .split(",")
      .sort();
    expect(sent).toEqual(["custom_status", "success"]);
  });

  it("the test button reports the key-free result", async () => {
    const fetchMock = mockFetch(ENV_VIEW, (url, init) => {
      if (init.method === "POST" && url.includes("/test")) {
        return { status: 200, body: { ok: true, detail: "test email delivered", latency_ms: 42 } };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);
    fireEvent.click(await screen.findByTestId("notify-test-email", undefined, { timeout: 5000 }));
    expect(await screen.findByTestId("notify-test-result-email")).toHaveTextContent(
      /test email delivered \(42 ms\)/,
    );
    const post = fetchMock.mock.calls.find(([, i]) => i?.method === "POST");
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({ channel: "email" });
  });

  it("surfaces a validation rejection without losing the form", async () => {
    const fetchMock = mockFetch(ENV_VIEW, (url, init) => {
      if (init.method === "PUT") {
        return { status: 400, body: "invalid notify settings: webhook_url must be http:// or https://" };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<NotificationsAdminPage />);
    fireEvent.change(await screen.findByTestId("notify-webhook-url", undefined, { timeout: 5000 }), {
      target: { value: "ftp://nope" },
    });
    fireEvent.click(screen.getByTestId("notify-save"));
    expect(await screen.findByRole("alert")).toHaveTextContent(/webhook_url must be/);
    expect(screen.getByTestId("notify-webhook-url")).toHaveValue("ftp://nope");
  });
});
