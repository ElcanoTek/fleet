import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

describe("SkillsPage workspace pack", () => {
  it("says the pack is empty instead of 'No skills match' when there is no query", async () => {
    vi.stubGlobal("fetch", mockFetch([]));
    render(<SkillsPage />);
    expect(
      await screen.findByText("No skills are installed on this deployment."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/No skills match/)).toBeNull();
  });

  it("never shows a superseded skill's body under the newly opened header", async () => {
    // Skill A's detail fetch is held open; B's resolves at once. Clicking A
    // then B must render B's body, and A's late arrival must be dropped.
    let releaseA: (() => void) | undefined;
    const gateA = new Promise<void>((r) => {
      releaseA = r;
    });
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === "/api/skills/alpha") {
        await gateA;
        return {
          ok: true,
          status: 200,
          json: async () => ({ name: "alpha", description: "", source: "builtin", content: "ALPHA BODY" }),
        };
      }
      if (url === "/api/skills/beta") {
        return {
          ok: true,
          status: 200,
          json: async () => ({ name: "beta", description: "", source: "builtin", content: "BETA BODY" }),
        };
      }
      if (url.startsWith("/api/skills")) {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            skills: [
              { name: "alpha", description: "First.", source: "builtin" },
              { name: "beta", description: "Second.", source: "builtin" },
            ],
          }),
        };
      }
      return { ok: true, status: 200, json: async () => ({ skills: [] }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SkillsPage />);
    const alpha = await screen.findByTestId("skill-row-alpha");
    const beta = screen.getByTestId("skill-row-beta");
    fireEvent.click(within(alpha).getByRole("button", { name: "View" }));
    fireEvent.click(within(beta).getByRole("button", { name: "View" }));
    expect(await within(beta).findByText("BETA BODY")).toBeInTheDocument();
    // A's response lands late: it must not replace B's body or appear anywhere.
    releaseA?.();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith("/api/skills/alpha", expect.anything()));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText("ALPHA BODY")).toBeNull();
    expect(within(beta).getByText("BETA BODY")).toBeInTheDocument();
  });
});

describe("SkillsPage failure states", () => {
  it("says the personal-skills read failed, with Retry, instead of loading forever", async () => {
    let userSkillsStatus = 500;
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url.startsWith("/api/user-skills")) {
        return userSkillsStatus === 200
          ? { ok: true, status: 200, json: async () => ({ skills: [{ id: "u1", name: "mine", description: "", body: "", status: "active" }] }) }
          : { ok: false, status: userSkillsStatus, json: async () => ({}), text: async () => "" };
      }
      return { ok: true, status: 200, json: async () => ({ skills: [] }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SkillsPage />);
    const failure = await screen.findByTestId("skills-mine-error");
    expect(failure).toHaveTextContent("Failed to load your skills: 500");
    // Neither "Loading…" nor the "No skills yet" empty state: the rows are
    // unknown, not absent.
    const yourSkills = screen.getByText("Your skills").closest("section, div") as HTMLElement;
    expect(within(yourSkills).queryByText(/No skills yet/)).toBeNull();

    userSkillsStatus = 200;
    fireEvent.click(within(failure).getByRole("button", { name: "Retry" }));
    expect(await screen.findByTestId("skill-row-mine")).toBeInTheDocument();
    expect(screen.queryByTestId("skills-mine-error")).toBeNull();
  });

  it("reports a failed SKILL.md read in the row instead of spinning", async () => {
    let detailStatus = 502;
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === "/api/skills/alpha") {
        return detailStatus === 200
          ? { ok: true, status: 200, json: async () => ({ name: "alpha", description: "", source: "builtin", content: "ALPHA BODY" }) }
          : { ok: false, status: detailStatus, json: async () => ({}) };
      }
      if (url.startsWith("/api/skills")) {
        return {
          ok: true,
          status: 200,
          json: async () => ({ skills: [{ name: "alpha", description: "First.", source: "builtin" }] }),
        };
      }
      return { ok: true, status: 200, json: async () => ({ skills: [] }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SkillsPage />);
    const alpha = await screen.findByTestId("skill-row-alpha");
    fireEvent.click(within(alpha).getByRole("button", { name: "View" }));
    const rowError = await within(alpha).findByRole("alert");
    expect(rowError).toHaveTextContent("Failed to load skill: 502");
    expect(within(alpha).queryByText("Loading…")).toBeNull();

    detailStatus = 200;
    fireEvent.click(within(rowError).getByRole("button", { name: "Retry" }));
    expect(await within(alpha).findByText("ALPHA BODY")).toBeInTheDocument();
  });

  it("matches the search against names and sources case-insensitively", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch([
        { name: "PDF-Extract", description: "Pull text out.", source: "Bundle" },
        { name: "deploy", description: "Ship it.", source: "builtin" },
      ]),
    );
    render(<SkillsPage />);
    await screen.findByText("PDF-Extract");
    fireEvent.change(screen.getByLabelText("Search workspace skills"), { target: { value: "pdf" } });
    expect(screen.getByText("PDF-Extract")).toBeInTheDocument();
    expect(screen.queryByText("deploy")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search workspace skills"), { target: { value: "bundle" } });
    expect(screen.getByText("PDF-Extract")).toBeInTheDocument();
  });
});
