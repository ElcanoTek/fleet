import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import FeaturesAdminPage from "./page";

// Settings → Admin → Features — the admin Features page. Load-bearing
// assertions: settings render grouped with provenance badges, a toggle PUTs
// and re-renders from the response, an enum change PUTs the picked option, an
// overridden row resets via DELETE, unknown server keys still render (never
// vanish), a server-side rejection surfaces as a row error, the filter hides
// empty groups, and the guardrail probe reports honestly.

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
  kind: "bool" | "int" | "enum" | "url" | "model";
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

const DEFAULT_TIER: Resolved = {
  key: "default_model",
  kind: "model",
  env_var: "FLEET_DEFAULT_MODEL",
  value: "google/gemini-3.8-flash",
  source: "default",
  default: "google/gemini-3.8-flash",
};

function mockFetch(
  settings: Resolved[],
  onWrite?: (url: string, init: RequestInit) => { status: number; body: unknown } | undefined,
) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
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

  // Model tiers (#1187): the row renders the combobox picker, saves only on
  // the explicit Save (a workspace-wide tier change must not fire per
  // keystroke), and any free-typed provider/model slug commits.
  it("saves a model tier via its picker only on explicit Save", async () => {
    const fetchMock = mockFetch([DEFAULT_TIER], (url, init) => {
      if (init.method === "PUT" && url.includes("default_model")) {
        return {
          status: 200,
          body: { ...DEFAULT_TIER, value: "acme/frontier-1", source: "admin" },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);

    expect(await screen.findByText("Model tiers")).toBeInTheDocument();
    const wrap = await screen.findByTestId("model-picker-default_model");
    const input = within(wrap).getByRole("combobox");
    expect(input).toHaveValue("google/gemini-3.8-flash");

    // No Save button until the value is dirty; typing alone fires no write.
    expect(screen.queryByTestId("save-default_model")).toBeNull();
    fireEvent.change(input, { target: { value: "acme/frontier-1" } });
    expect(fetchMock.mock.calls.filter(([, i]) => i?.method === "PUT")).toHaveLength(0);
    fireEvent.click(screen.getByTestId("save-default_model"));

    await waitFor(() => expect(input).toHaveValue("acme/frontier-1"));
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(put?.[0]).toBe("/api/admin/settings/default_model");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({ value: "acme/frontier-1" });
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

    // The guardrail action block keeps the Privacy group alive via its own
    // search text even when every setting row is filtered out.
    fireEvent.change(filter, { target: { value: "detector" } });
    expect(screen.getByText("Privacy & data protection")).toBeInTheDocument();
    expect(screen.getByTestId("guardrail-probe-run")).toBeInTheDocument();
    expect(screen.queryByText("PII redaction")).toBeNull();

    fireEvent.change(filter, { target: { value: "zzz-no-match" } });
    expect(screen.getByText(/No settings match/)).toBeInTheDocument();
  });

  it("saves a url setting via its Save button and runs the guardrail probe", async () => {
    const GUARDRAIL_URL: Resolved = {
      key: "guardrail_url",
      kind: "url",
      env_var: "FLEET_GUARDRAIL_URL",
      value: "",
      source: "default",
      default: "",
    };
    const fetchMock = mockFetch([PII, GUARDRAIL_URL], (url, init) => {
      if (init.method === "PUT" && url.includes("guardrail_url")) {
        return {
          status: 200,
          body: { ...GUARDRAIL_URL, value: "http://127.0.0.1:8790/v1/check", source: "admin" },
        };
      }
      if (init.method === "POST" && url.includes("guardrail/test")) {
        return {
          status: 200,
          body: {
            ok: true,
            mode: "observe",
            profile: "prompt-injection",
            flagged: true,
            score: 0.97,
            detail: "instruction override",
            latency_ms: 12,
          },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);

    const input = await screen.findByTestId("input-guardrail_url");
    fireEvent.change(input, { target: { value: "http://127.0.0.1:8790/v1/check" } });
    fireEvent.click(screen.getByTestId("save-guardrail_url"));
    await waitFor(() => expect(screen.getByText("Overridden")).toBeInTheDocument());
    const put = fetchMock.mock.calls.find(([, i]) => i?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({
      value: "http://127.0.0.1:8790/v1/check",
    });

    // Probe: reports profile + mode + verdict + detail.
    fireEvent.click(screen.getByTestId("guardrail-probe-run"));
    const probe = screen.getByTestId("guardrail-probe");
    await waitFor(() => expect(probe).toHaveTextContent(/prompt-injection \(observe\)/));
    expect(probe).toHaveTextContent(/flagged/);
    expect(probe).toHaveTextContent(/instruction override/);
  });

  it("surfaces a probe failure (dead guardrail detector) honestly", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (init.method === "POST" && url.includes("guardrail/test")) {
        return {
          status: 200,
          body: {
            ok: false,
            mode: "block",
            profile: "prompt-injection",
            flagged: false,
            detail: "detector unreachable: connection refused",
            latency_ms: 3,
          },
        };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    fireEvent.click(await screen.findByTestId("guardrail-probe-run"));
    const probe = screen.getByTestId("guardrail-probe");
    await waitFor(() => expect(probe).toHaveTextContent(/unreachable/));
  });

  it("disables only the saving row while its write is in flight", async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (!init || init.method === undefined || init.method === "GET") {
        return { ok: true, status: 200, json: async () => ({ settings: [SUBAGENTS, PII] }) };
      }
      // Hold the PUT open so the in-flight state is observable.
      await gate;
      return {
        ok: true,
        status: 200,
        json: async () => ({ ...SUBAGENTS, value: "true", source: "admin" }),
        text: async () => "{}",
      };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    const toggle = await screen.findByTestId("toggle-subagents_enabled");
    fireEvent.click(toggle);
    await waitFor(() => expect(screen.getByTestId("toggle-subagents_enabled")).toBeDisabled());
    // The sibling row is untouched by the pending write.
    const pii = screen.getByTestId("setting-pii_redaction_mode");
    for (const b of within(pii).getAllByRole("button")) expect(b).not.toBeDisabled();
    release?.();
    await waitFor(() =>
      expect(screen.getByTestId("toggle-subagents_enabled")).not.toBeDisabled(),
    );
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

describe("FeaturesAdminPage reload", () => {
  it("Retry after a failed load renders the fresh server list", async () => {
    let fail = true;
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (!init || init.method === undefined || init.method === "GET") {
        if (fail) return { ok: false, status: 502, json: async () => ({}), text: async () => "" };
        return { ok: true, status: 200, json: async () => ({ settings: [SUBAGENTS] }) };
      }
      return { ok: true, status: 200, json: async () => ({}), text: async () => "{}" };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeaturesAdminPage />);
    const retry = await screen.findByRole("button", { name: "Retry" });
    fail = false;
    fireEvent.click(retry);
    // The refetched list is what renders — nothing local layered over it.
    const toggle = await screen.findByTestId("toggle-subagents_enabled");
    expect(toggle).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText("Server default")).toBeInTheDocument();
  });
});
