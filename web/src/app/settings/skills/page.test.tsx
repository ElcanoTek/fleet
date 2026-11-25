import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import SkillsPage from "./page";
import { jsonResponse, textResponse } from "@/test/fetch";
import { _resetMeForTests } from "@/app/shared/lib/account";

// SkillsPage — declared allowed-tools are surfaced for REVIEW as an advisory
// signal, never as an enforced boundary. Load-bearing: a skill that declares
// allowed-tools shows them with the "(advisory)" qualifier; a skill without
// them shows no tools line.

function mockFetch(skills: unknown[]) {
  return vi.fn().mockImplementation(async (url: string) => {
    if (url.startsWith("/api/skills")) {
      return jsonResponse({ skills });
    }
    // user-skills + anything else: empty.
    return jsonResponse({ skills: [] });
  });
}

afterEach(() => {
  cleanup();
  _resetMeForTests();
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
        return jsonResponse({ name: "alpha", description: "", source: "builtin", content: "ALPHA BODY" });
      }
      if (url === "/api/skills/beta") {
        return jsonResponse({ name: "beta", description: "", source: "builtin", content: "BETA BODY" });
      }
      if (url.startsWith("/api/skills")) {
        return jsonResponse({
            skills: [
              { name: "alpha", description: "First.", source: "builtin" },
              { name: "beta", description: "Second.", source: "builtin" },
            ],
          });
      }
      return jsonResponse({ skills: [] });
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

describe("SkillsPage new-skill form", () => {
  // The form opens from a button that stays put, so the cursor has to be
  // moved into it; and its greyed-out button says what it is waiting for.
  it("puts the cursor in the name field and names the missing field", async () => {
    vi.stubGlobal("fetch", mockFetch([]));
    render(<SkillsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "New skill" }));
    const name = screen.getByPlaceholderText("my-skill");
    expect(name).toHaveFocus();
    const create = screen.getByRole("button", { name: "Create skill" });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription("To continue, fill in: Name");

    fireEvent.change(name, { target: { value: "deal-check" } });
    expect(create).toBeEnabled();
    expect(screen.queryByText(/To continue, fill in/)).toBeNull();
  });
});

describe("SkillsPage failure states", () => {
  it("says the personal-skills read failed, with Retry, instead of loading forever", async () => {
    let userSkillsStatus = 500;
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url.startsWith("/api/user-skills")) {
        return userSkillsStatus === 200
          ? jsonResponse({ skills: [{ id: "u1", name: "mine", description: "", body: "", status: "active" }] })
          : textResponse("", userSkillsStatus);
      }
      return jsonResponse({ skills: [] });
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
          ? jsonResponse({ name: "alpha", description: "", source: "builtin", content: "ALPHA BODY" })
          : jsonResponse({}, detailStatus);
      }
      if (url.startsWith("/api/skills")) {
        return jsonResponse({ skills: [{ name: "alpha", description: "First.", source: "builtin" }] });
      }
      return jsonResponse({ skills: [] });
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

  it("badges workspace-shared teammate skills and supports 1-click sharing and opt-in installation", async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === "/api/user-skills/u1" && init?.method === "PUT") {
        return jsonResponse({});
      }
      if (url === "/api/user-skills" && init?.method === "POST") {
        return jsonResponse({ id: "u2" });
      }
      if (url === "/api/skills/team-opt-log") {
        return jsonResponse({
            name: "team-opt-log",
            description: "Shared optimization workflow",
            source: "workspace",
            body: "1. Open workbench.",
            content: "---\nname: team-opt-log\ndescription: Shared optimization workflow\n---\n\n1. Open workbench.",
          });
      }
      if (url.startsWith("/api/user-skills")) {
        return jsonResponse({
            skills: [
              {
                id: "u1",
                name: "my-opt-log",
                description: "Optimization log workflow",
                body: "1. Run review.",
                status: "active",
                visibility: "private",
              },
            ],
          });
      }
      return jsonResponse({
          skills: [],
          shared_skills: [
            {
              name: "team-opt-log",
              description: "Shared optimization workflow",
              source: "workspace",
              shared_by: "brendan.dahlgren@elcanotek.com",
            },
          ],
        });
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SkillsPage />);
    expect(await screen.findByText("Shared: brendan.dahlgren@elcanotek.com")).toBeInTheDocument();
    const myRow = await screen.findByTestId("skill-row-my-opt-log");
    fireEvent.click(within(myRow).getByRole("button", { name: "Share with workspace" }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/user-skills/u1",
        expect.objectContaining({
          method: "PUT",
          body: expect.stringContaining('"visibility":"workspace"'),
        }),
      ),
    );
    const sharedRow = await screen.findByTestId("skill-row-team-opt-log");
    fireEvent.click(within(sharedRow).getByRole("button", { name: "Add to my skills" }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/user-skills",
        expect.objectContaining({
          method: "POST",
          body: expect.stringContaining('"name":"team-opt-log"'),
        }),
      ),
    );

    // Clicking "Clone & edit" on a Workspace pack skill opens the editor pre-filled with its content and workspace sharing enabled.
    fireEvent.click(within(sharedRow).getByRole("button", { name: "Clone & edit" }));
    const nameInput = (await screen.findByLabelText(/Name \(lowercase-kebab/i)) as HTMLInputElement;
    await waitFor(() => expect(nameInput.value).toBe("team-opt-log"));
    expect((screen.getByLabelText(/Instructions \(markdown/i) as HTMLTextAreaElement).value).toBe(
      "1. Open workbench.",
    );
  });
});

// "On for everyone": an admin can switch a workspace-shared skill on for the
// whole workspace; everyone else sees it as already on, with nothing to add.
describe("SkillsPage skills that are on for everyone", () => {
  function forAllFetch(admin: boolean) {
    return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === "/api/me") {
        return jsonResponse({ email: "u@x.com", role: admin ? "admin" : "member", team_id: "", admin });
      }
      if (url === "/api/user-skills/u1" && init?.method === "PUT") {
        return jsonResponse({});
      }
      if (url.startsWith("/api/user-skills")) {
        return jsonResponse({
          skills: [
            {
              id: "u1",
              name: "my-deal-check",
              description: "Check a deal sheet",
              body: "1. Check totals.",
              status: "active",
              visibility: "workspace",
            },
            {
              id: "u2",
              name: "my-private",
              description: "Private notes",
              body: "Notes.",
              status: "active",
              visibility: "private",
            },
          ],
        });
      }
      return jsonResponse({
        skills: [],
        shared_skills: [
          {
            name: "team-qa",
            description: "Team QA workflow",
            source: "workspace",
            shared_by: "admin@x.com",
            enabled_for_all: true,
          },
          { name: "team-optional", description: "Optional workflow", source: "workspace", shared_by: "a@x.com" },
        ],
      });
    });
  }

  it("lets an admin turn a shared skill on for everyone, and shows such a skill as already on", async () => {
    const fetchMock = forAllFetch(true);
    vi.stubGlobal("fetch", fetchMock);
    render(<SkillsPage />);

    const myRow = await screen.findByTestId("skill-row-my-deal-check");
    fireEvent.click(await within(myRow).findByRole("button", { name: "Turn on for everyone" }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/user-skills/u1",
        expect.objectContaining({
          method: "PUT",
          body: expect.stringContaining('"enabled_for_all":true'),
        }),
      ),
    );
    // Only a workspace-shared skill offers it.
    const privateRow = screen.getByTestId("skill-row-my-private");
    expect(within(privateRow).queryByRole("button", { name: "Turn on for everyone" })).toBeNull();

    // A skill that is on for everyone is badged and has nothing to add; an
    // ordinary shared one keeps "Add to my skills".
    const onRow = await screen.findByTestId("skill-row-team-qa");
    expect(within(onRow).getByText("On for everyone")).toBeInTheDocument();
    expect(within(onRow).queryByRole("button", { name: "Add to my skills" })).toBeNull();
    expect(within(onRow).getByRole("button", { name: "Clone & edit" })).toBeInTheDocument();
    const optionalRow = screen.getByTestId("skill-row-team-optional");
    expect(within(optionalRow).getByRole("button", { name: "Add to my skills" })).toBeInTheDocument();
  });

  it("does not offer the switch to someone who is not an admin", async () => {
    vi.stubGlobal("fetch", forAllFetch(false));
    render(<SkillsPage />);
    const myRow = await screen.findByTestId("skill-row-my-deal-check");
    expect(await within(myRow).findByRole("button", { name: "Unshare" })).toBeInTheDocument();
    expect(within(myRow).queryByRole("button", { name: "Turn on for everyone" })).toBeNull();
  });
});
