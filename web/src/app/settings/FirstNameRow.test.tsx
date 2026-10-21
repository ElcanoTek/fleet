import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { FirstNameRow } from "./FirstNameRow";

// Settings → General → "Your first name" (ADR-0087): read the saved name from
// GET /api/me/name, save or clear it with PUT, and show the server's refusal
// text as-is. The row never fills the field from the email.

type Call = { url: string; method: string; body?: string };

function mockFetch(impl: (url: string, init?: RequestInit) => Response) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), method: init?.method ?? "GET", body: init?.body as string | undefined });
      return Promise.resolve(impl(String(input), init));
    }),
  );
  return calls;
}

const me = (givenName: string) =>
  new Response(
    JSON.stringify({ email: "elyse.messer@x.com", role: "member", team_id: "", admin: false, given_name: givenName }),
    { status: 200 },
  );

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("FirstNameRow", () => {
  it("shows an unset name as empty, never one guessed from the email", async () => {
    mockFetch(() => me(""));
    render(<FirstNameRow />);
    const input = await screen.findByLabelText("Your first name");
    await waitFor(() => expect(input).not.toBeDisabled());
    expect((input as HTMLInputElement).value).toBe("");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("saves a trimmed name with PUT", async () => {
    let saved = "";
    const calls = mockFetch((_url, init) => {
      if (init?.method === "PUT") {
        saved = (JSON.parse(String(init.body)) as { given_name: string }).given_name;
      }
      return me(saved);
    });
    render(<FirstNameRow />);
    const input = await screen.findByLabelText("Your first name");
    await waitFor(() => expect(input).not.toBeDisabled());

    fireEvent.change(input, { target: { value: "  Elyse " } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const put = calls.find((c) => c.method === "PUT");
    expect(put?.url).toBe("/api/me/name");
    expect(JSON.parse(put?.body ?? "{}")).toEqual({ given_name: "Elyse" });
    expect((input as HTMLInputElement).value).toBe("Elyse");
  });

  it("clears the name with an empty value", async () => {
    let saved = "Elyse";
    mockFetch((_url, init) => {
      if (init?.method === "PUT") saved = "";
      return me(saved);
    });
    render(<FirstNameRow />);
    const input = await screen.findByLabelText("Your first name");
    await waitFor(() => expect((input as HTMLInputElement).value).toBe("Elyse"));

    fireEvent.change(input, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Cleared.")).toBeInTheDocument();
  });

  it("offers a retry when the first read fails, and redirects a lapsed session", async () => {
    let fail = true;
    mockFetch(() => (fail ? new Response(JSON.stringify({ error: "chat server unavailable" }), { status: 502 }) : me("Elyse")));
    render(<FirstNameRow />);
    expect(await screen.findByText("chat server unavailable")).toBeInTheDocument();
    expect(screen.getByLabelText("Your first name")).toBeDisabled();

    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    const input = screen.getByLabelText("Your first name") as HTMLInputElement;
    await waitFor(() => expect(input.value).toBe("Elyse"));
    expect(input).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });

  it("explains a read-only refusal instead of showing the raw body", async () => {
    mockFetch((_url, init) =>
      init?.method === "PUT" ? new Response(JSON.stringify({ error: "read_only" }), { status: 403 }) : me(""),
    );
    render(<FirstNameRow />);
    const input = await screen.findByLabelText("Your first name");
    await waitFor(() => expect(input).not.toBeDisabled());

    fireEvent.change(input, { target: { value: "Eve" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/read-only/)).toBeInTheDocument();
    expect(screen.queryByText(/read_only/)).toBeNull();
  });

  it("shows the server's refusal and keeps the draft", async () => {
    mockFetch((_url, init) =>
      init?.method === "PUT"
        ? new Response("use letters, single spaces, apostrophes, hyphens or periods", { status: 400 })
        : me(""),
    );
    render(<FirstNameRow />);
    const input = await screen.findByLabelText("Your first name");
    await waitFor(() => expect(input).not.toBeDisabled());

    fireEvent.change(input, { target: { value: "elyse@x.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/use letters, single spaces/)).toBeInTheDocument();
    expect((input as HTMLInputElement).value).toBe("elyse@x.com");
  });
});
