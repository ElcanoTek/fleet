import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { FeatureSettingsPanel } from "./FeatureSettingsPanel";

// FeatureSettingsPanel — the admin Features panel. Load-bearing assertions:
// settings render grouped with provenance chips, a toggle PUTs and re-renders
// from the response, an enum change PUTs the picked option, a customized row
// resets via DELETE, unknown server keys still render (never vanish), and a
// server-side rejection surfaces as a row error.

type Resolved = {
  key: string;
  kind: "bool" | "int" | "enum";
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

describe("FeatureSettingsPanel", () => {
  it("renders settings grouped with provenance and env-var attribution", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([PII, { ...SUBAGENTS, value: "true", source: "admin", updated_by: "boss@x.com" }]),
    );
    render(<FeatureSettingsPanel />);
    expect(await screen.findByText("PII redaction")).toBeInTheDocument();
    expect(screen.getByText("Privacy & data protection")).toBeInTheDocument();
    expect(screen.getByText("Sub-agent delegation")).toBeInTheDocument();
    // Provenance: one customized row (with reset + attribution), one default.
    expect(screen.getByText("Customized")).toBeInTheDocument();
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
    render(<FeatureSettingsPanel />);
    const toggle = await screen.findByTestId("toggle-subagents_enabled");
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "true"));
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(put?.[0]).toBe("/api/admin/settings/subagents_enabled");
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({ value: "true" });
    expect(screen.getByText("Customized")).toBeInTheDocument();
  });

  it("picking an enum option PUTs it and shows the option help", async () => {
    const fetchMock = mockFetch([PII], (url, init) => {
      if (init.method === "PUT" && url.includes("pii_redaction_mode")) {
        return { status: 200, body: { ...PII, value: "redact", source: "admin" } };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<FeatureSettingsPanel />);
    fireEvent.click(await screen.findByTestId("option-pii_redaction_mode-redact"));
    await waitFor(() =>
      expect(screen.getByTestId("option-pii_redaction_mode-redact")).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );
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
    render(<FeatureSettingsPanel />);
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
    render(<FeatureSettingsPanel />);
    fireEvent.click(await screen.findByTestId("option-pii_redaction_mode-block"));
    expect(await screen.findByRole("alert")).toHaveTextContent(/invalid setting value/);
    expect(screen.getByTestId("option-pii_redaction_mode-off")).toHaveAttribute(
      "aria-checked",
      "true",
    );
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
    render(<FeatureSettingsPanel />);
    expect(await screen.findByText("Other")).toBeInTheDocument();
    expect(screen.getByText("Future feature enabled")).toBeInTheDocument();
    expect(screen.getByTestId("toggle-future_feature_enabled")).toBeInTheDocument();
  });

  it("surfaces a stale (ignored) override with a warning chip and a Reset", async () => {
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
    render(<FeatureSettingsPanel />);
    expect(await screen.findByText("Ignored override")).toBeInTheDocument();
    expect(screen.getByText(/outside this setting.s current bounds/)).toBeInTheDocument();
    // The ignored row is still resettable.
    fireEvent.click(screen.getByTestId("reset-tool_disclosure_threshold"));
    await waitFor(() => expect(screen.queryByText("Ignored override")).toBeNull());
    expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(true);
  });

  it("reports the admin-allowlist 403 instead of an empty panel", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 403, json: async () => ({}) }),
    );
    render(<FeatureSettingsPanel />);
    expect(await screen.findByText(/not on the admin allowlist/)).toBeInTheDocument();
  });
});
