import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import FeaturesAdminPage from "./page";

// Settings → Admin → Features — the admin Features page. Load-bearing
// assertions: settings render grouped with provenance badges, a toggle PUTs
// and re-renders from the response, an enum change PUTs the picked option, an
// overridden row resets via DELETE, unknown server keys still render (never
// vanish), a server-side rejection surfaces as a row error, the filter hides
// empty groups, and the Rampart probe/install flows report honestly.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; force "admin" so the page renders. (The real
// hook probes an admin endpoint; authorization stays server-side regardless.)
vi.mock("../../useIsAdmin", () => ({
  useIsAdmin: () => "admin",
}));

type Resolved = {
  key: string;
  kind: "bool" | "int" | "enum" | "url";
  enum?: string[];
  min?: number;
  max?: number;
  min_zero_ok?: boolean;
  env_var: string;
  value: string;
  source: "admin" | "default";
  default: string;
  updated_by?: string;
  stale?: boolean;
};

const PII: Resolved = {
  key: "pii_redaction_mode",
  kind: "enum",
  enum: ["off", "observe", "redact", "block"],
  env_var: "FLEET_PII_REDACTION_ENABLED / FLEET_PII_REDACTION_MODE",
  value: "off",
  source: "default",
  default: "off",
};

const SUBAGENTS: Resolved = {
  key: "subagents_enabled",
  kind: "bool",
  env_var: "FLEET_SUBAGENTS_ENABLED",
  value: "false",
  source: "default",
  default: "false",
};

const THRESHOLD: Resolved = {
  key: "tool_disclosure_threshold",
  kind: "int",
  min: 1,
  max: 100000,
  env_var: "FLEET_TOOL_DISCLOSURE_THRESHOLD",
  value: "128",
  source: "default",
  default: "128",
};

function mockFetch(
  settings: Resolved[],
  onWrite?: (url: string, init: RequestInit) => { status: number; body: unknown } | undefined,
) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    // The Rampart install manager polls this on mount; default to "not
    // wired" (501) so it renders nothing unless a test opts in.
    if (url.includes("/pii-redaction/install")) {
      const custom = onWrite?.(url, init ?? { method: "GET" });
      if (custom) {
        return {
          ok: custom.status < 400,
          status: custom.status,
          json: async () => custom.body,
          text: async () => JSON.stringify(custom.body),
        };
      }
      return { ok: false, status: 501, json: async () => ({}), text: async () => "" };
    }
    if (!init || init.method === undefined || init.method === "GET") {
      return { ok: true, status: 200, json: async () => ({ settings }) };
    }
    const custom = onWrite?.(url, init);
    if (custom) {
      return {
        ok: custom.status < 400,
        status: custom.status,
        json: async () => custom.body,
        text: async () => JSON.stringify(custom.body),
      };
    }
    return { ok: true, status: 200, json: async () => ({}), text: async () => "{}" };
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("FeaturesAdminPage", () => {
  it("renders settings grouped with provenance and env-var attribution", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([PII, { ...SUBAGENTS, value: "true", source: "admin", updated_by: "boss@x.com" }]),
    );
    render(<FeaturesAdminPage />);
    expect(await screen.findByText("PII redaction")).toBeInTheDocument();
    expect(screen.getByText("Privacy & data protection")).toBeInTheDocument();
    expect(screen.getByText("Sub-agent delegation")).toBeInTheDocument();
    // Provenance: one overridden row (with reset + attribution), one default.
    expect(screen.getByText("Overridden")).toBeInTheDocument();
    expect(screen.getByText("Server default")).toBeInTheDocument();
    expect(screen.getByText(/set by boss@x.com/)).toBeInTheDocument();
    expect(screen.getByText("FLEET_SUBAGENTS_ENABLED")).toBeInTheDocument();
    expect(screen.getByTestId("reset-subagents_enabled")).toBeInTheDocument();
  });

  it("toggling a bool PUTs the flipped value and re-renders from the response", async () => {
    const fetchMock = mockFetch([SUBAGENTS], (url, init) => {
      if (init.method === "PUT" && url.includes("subagents_enabled")) {
        return {
          status: 200,
          body: { ...SUBAGENTS, value: "true", source: "admin", updated_by: "boss@x.com" },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    const toggle = await screen.findByTestId("toggle-subagents_enabled");
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "true"));
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(put?.[0]).toBe("/api/admin/settings/subagents_enabled");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({ value: "true" });
    expect(screen.getByText("Overridden")).toBeInTheDocument();
  });

  it("picking an enum option PUTs it and shows the option help", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (init.method === "PUT" && url.includes("pii_redaction_mode")) {
        return { status: 200, body: { ...PII, value: "redact", source: "admin" } };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Redact" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Redact" })).toHaveAttribute(
        "aria-pressed",
        "true",
      ),
    );
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({ value: "redact" });
    expect(screen.getByText(/\[PII:kind\] marker/)).toBeInTheDocument();
  });

  it("saving an int only on explicit Save, and reset DELETEs", async () => {
    const customized: Resolved = { ...THRESHOLD, value: "40", source: "admin" };
    const fetchMock = mockFetch([customized], (url, init) => {
      if (init.method === "DELETE" && url.includes("tool_disclosure_threshold")) {
        return { status: 200, body: THRESHOLD };
      }
      if (init.method === "PUT") {
        return { status: 200, body: { ...customized, value: "64" } };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    const input = await screen.findByTestId("input-tool_disclosure_threshold");
    // No Save button until the value is dirty; typing alone fires no write.
    expect(screen.queryByTestId("save-tool_disclosure_threshold")).toBeNull();
    fireEvent.change(input, { target: { value: "64" } });
    expect(fetchMock.mock.calls.filter(([, i]) => i?.method === "PUT")).toHaveLength(0);
    fireEvent.click(screen.getByTestId("save-tool_disclosure_threshold"));
    await waitFor(() => expect(input).toHaveValue(64));

    // Reset reverts to the default-sourced row from the DELETE response.
    fireEvent.click(screen.getByTestId("reset-tool_disclosure_threshold"));
    await waitFor(() => expect(screen.getByText("Server default")).toBeInTheDocument());
    const del = fetchMock.mock.calls.find(([, i]) => i?.method === "DELETE");
    expect(del?.[0]).toBe("/api/admin/settings/tool_disclosure_threshold");
  });

  it("surfaces a server rejection as a row error and keeps the old value", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (init.method === "PUT") {
        return { status: 400, body: "invalid setting value" };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Block" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/invalid setting value/);
    expect(screen.getByRole("button", { name: "Off" })).toHaveAttribute("aria-pressed", "true");
  });

  it("renders unknown server keys under Other so new settings never vanish", async () => {
    const unknown: Resolved = {
      key: "future_feature_enabled",
      kind: "bool",
      env_var: "FLEET_FUTURE_FEATURE_ENABLED",
      value: "false",
      source: "default",
      default: "false",
    };
    vi.stubGlobal("fetch", mockFetch([PII, unknown]));
    render(<FeaturesAdminPage />);
    expect(await screen.findByText("Other")).toBeInTheDocument();
    expect(screen.getByText("Future feature enabled")).toBeInTheDocument();
    expect(screen.getByTestId("toggle-future_feature_enabled")).toBeInTheDocument();
  });

  it("surfaces a stale (ignored) override with a warning note and a Reset", async () => {
    const staleRow: Resolved = {
      ...THRESHOLD,
      source: "default",
      stale: true,
      updated_by: "old-admin@x.com",
    };
    const fetchMock = mockFetch([staleRow], (url, init) => {
      if (init.method === "DELETE") {
        return { status: 200, body: THRESHOLD };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    expect(
      await screen.findByText(/outside this setting.s current bounds/),
    ).toBeInTheDocument();
    expect(screen.getByText(/set by old-admin@x.com/)).toBeInTheDocument();
    // The ignored row is still resettable.
    fireEvent.click(screen.getByTestId("reset-tool_disclosure_threshold"));
    await waitFor(() =>
      expect(screen.queryByText(/outside this setting.s current bounds/)).toBeNull(),
    );
    expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(true);
  });

  it("filters settings live and hides groups with nothing visible", async () => {
    vi.stubGlobal("fetch", mockFetch([PII, SUBAGENTS, THRESHOLD]));
    render(<FeaturesAdminPage />);
    await screen.findByText("PII redaction");

    const filter = screen.getByLabelText("Filter settings");
    fireEvent.change(filter, { target: { value: "sub-agent" } });
    expect(screen.getByText("Sub-agent delegation")).toBeInTheDocument();
    expect(screen.queryByText("PII redaction")).toBeNull();
    expect(screen.queryByText("Privacy & data protection")).toBeNull();

    // The Rampart action block keeps the Privacy group alive via its own
    // search text even when every setting row is filtered out.
    fireEvent.change(filter, { target: { value: "podman" } });
    expect(screen.getByText("Privacy & data protection")).toBeInTheDocument();
    expect(screen.getByTestId("pii-probe-run")).toBeInTheDocument();
    expect(screen.queryByText("PII redaction")).toBeNull();

    fireEvent.change(filter, { target: { value: "zzz-no-match" } });
    expect(screen.getByText(/No settings match/)).toBeInTheDocument();
  });

  it("saves a url setting via its Save button and runs the detection probe", async () => {
    const RAMPART_URL: Resolved = {
      key: "pii_rampart_url",
      kind: "url",
      env_var: "FLEET_PII_RAMPART_URL",
      value: "",
      source: "default",
      default: "",
    };
    const fetchMock = mockFetch([PII, RAMPART_URL], (url, init) => {
      if (init.method === "PUT" && url.includes("pii_rampart_url")) {
        return {
          status: 200,
          body: { ...RAMPART_URL, value: "http://127.0.0.1:8787/v1/redact", source: "admin" },
        };
      }
      if (init.method === "POST" && url.includes("pii-redaction/test")) {
        return {
          status: 200,
          body: {
            ok: true,
            engine: "rampart",
            mode: "redact",
            detail: "name×1, ssn×1",
            redacted: "Contact [GIVEN_NAME_1] ...",
            latency_ms: 12,
          },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);

    const input = await screen.findByTestId("input-pii_rampart_url");
    fireEvent.change(input, { target: { value: "http://127.0.0.1:8787/v1/redact" } });
    fireEvent.click(screen.getByTestId("save-pii_rampart_url"));
    await waitFor(() => expect(screen.getByText("Overridden")).toBeInTheDocument());
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({
      value: "http://127.0.0.1:8787/v1/redact",
    });

    // Probe: reports engine + findings + redacted preview.
    fireEvent.click(screen.getByTestId("pii-probe-run"));
    const result = await screen.findByTestId("pii-probe-result");
    expect(result).toHaveTextContent(/rampart engine \(redact\)/);
    expect(result).toHaveTextContent(/name×1, ssn×1/);
    expect(result).toHaveTextContent(/\[GIVEN_NAME_1\]/);
  });

  it("surfaces a probe failure (dead rampart service) honestly", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (init.method === "POST" && url.includes("pii-redaction/test")) {
        return {
          status: 200,
          body: {
            ok: false,
            engine: "rampart",
            mode: "redact",
            detail: "rampart service unreachable: connection refused (tool calls fall back to the pattern engine)",
            latency_ms: 3,
          },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    fireEvent.click(await screen.findByTestId("pii-probe-run"));
    const result = await screen.findByTestId("pii-probe-result");
    expect(result).toHaveTextContent(/unreachable/);
    expect(result).toHaveTextContent(/fall back to the pattern engine/);
  });

  it("offers one-click Rampart install and shows the running service", async () => {
    let installed = false;
    const fetchMock = mockFetch([PII], (url, init) => {
      if (!url.includes("/pii-redaction/install")) return undefined;
      if (init.method === "POST") {
        installed = true;
        return { status: 200, body: { state: "done", log: ["done"], container_running: true, url: "http://127.0.0.1:8787/v1/redact" } };
      }
      // GET status poll.
      return installed
        ? { status: 200, body: { state: "done", log: ["done"], container_running: true, url: "http://127.0.0.1:8787/v1/redact" } }
        : { status: 200, body: { state: "idle", log: [], container_running: false } };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);

    const btn = await screen.findByTestId("pii-install-run", undefined, { timeout: 5000 });
    fireEvent.click(btn);
    await waitFor(() => expect(screen.getByText("Service installed")).toBeInTheDocument());
    expect(screen.getByText("http://127.0.0.1:8787/v1/redact")).toBeInTheDocument();
    const post = fetchMock.mock.calls.find(
      ([u, i]) => String(u).includes("/pii-redaction/install") && (i as RequestInit)?.method === "POST",
    );
    expect(post).toBeTruthy();
  });

  it("removes the managed container only after inline confirmation", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (!url.includes("/pii-redaction/install")) return undefined;
      if (init.method === "DELETE") {
        return { status: 200, body: { state: "idle", log: [], container_running: false } };
      }
      // GET status: a running managed container.
      return {
        status: 200,
        body: { state: "done", log: ["done"], container_running: true, url: "http://127.0.0.1:8787/v1/redact" },
      };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);

    const remove = await screen.findByTestId("pii-install-remove", undefined, { timeout: 5000 });
    // First click arms; no DELETE yet.
    fireEvent.click(remove);
    expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false);
    expect(remove).toHaveTextContent("Confirm remove");
    // Second click fires the DELETE and the affordance returns to install.
    fireEvent.click(remove);
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(true),
    );
    await waitFor(() => expect(screen.getByTestId("pii-install-run")).toBeInTheDocument());
  });

  it("hides the install affordance when the installer is not wired (501)", async () => {
    vi.stubGlobal("fetch", mockFetch([PII])); // default install stub = 501
    render(<FeaturesAdminPage />);
    await screen.findByText("PII redaction");
    await waitFor(() => expect(screen.queryByTestId("pii-install")).toBeNull());
  });

  it("reports the admin-allowlist 403 instead of an empty panel", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 403, json: async () => ({}) }),
    );
    render(<FeaturesAdminPage />);
    expect(await screen.findByText(/not on the admin allowlist/)).toBeInTheDocument();
  });
});
