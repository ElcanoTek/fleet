import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import SkillsPage from "./page";

// SkillsPage — declared allowed-tools are surfaced for REVIEW as an advisory
// signal, never as an enforced boundary. Load-bearing: a skill that declares
// allowed-tools shows them with the "(advisory)" qualifier; a skill without
// them shows no tools line.

function mockFetch(skills: unknown[]) {
  return vi.fn().mockImplementation(async (url: string) => {
    if (url.startsWith("/api/skills")) {
      return { ok: true, status: 200, json: async () => ({ skills }) };
    }
    // user-skills + anything else: empty.
    return { ok: true, status: 200, json: async () => ({ skills: [] }) };
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("SkillsPage declared allowed-tools", () => {
  it("surfaces a skill's declared tools as an advisory signal", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([
        {
          name: "pdf-extract",
          description: "Pull text out of a PDF.",
          source: "bundle",
          declared_allowed_tools: ["bash", "run_python"],
        },
      ]),
    );
    render(<SkillsPage />);
    expect(await screen.findByText("pdf-extract")).toBeInTheDocument();
    // The declared tools appear, explicitly marked advisory (not enforced).
    expect(screen.getByText(/bash, run_python/)).toBeInTheDocument();
    expect(screen.getByText(/\(advisory\)/)).toBeInTheDocument();
  });

  it("badges a skill that came from an Agent Plugin with the plugin name", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([
        { name: "deploy", description: "Ship it.", source: "plugin", plugin: "acme-tools" },
      ]),
    );
    render(<SkillsPage />);
    expect(await screen.findByText("deploy")).toBeInTheDocument();
    expect(screen.getByText("Plugin: acme-tools")).toBeInTheDocument();
  });

  it("shows no tools line for a skill that declares none", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([{ name: "plain", description: "No tools declared.", source: "builtin" }]),
    );
    render(<SkillsPage />);
    expect(await screen.findByText("plain")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(/\(advisory\)/)).toBeNull());
  });
});
