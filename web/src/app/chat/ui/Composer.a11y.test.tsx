import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useEffect, useRef, useState } from "react";
import { Composer } from "./Composer";

// Focus + Escape behavior of the composer's model picker.
//
// The search field used to be focused by `autoFocus`; the jsx-a11y pass moved
// that onto an effect keyed on the picker being open. Escape is still handled
// on the (now presentational) wrapper around the chip + popover, because it has
// to work from the trigger button AND from the search field. Both are behavior
// a linter change could silently take away, so both are asserted here.

const MODELS = [
  { slug: "vendor/one", name: "Model One" },
  { slug: "vendor/two", name: "Model Two" },
];

// A minimal host that owns the state Composer normally gets from
// ChatExperience — enough for the model picker to open, focus and close.
// `overrides` lets a test hand in a tools roster and spy handlers.
function Host({
  overrides,
}: {
  overrides?: Partial<Parameters<typeof Composer>[0]>;
}) {
  const [prompt, setPrompt] = useState("");
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [modelSearchQuery, setModelSearchQuery] = useState("");
  const [selectedModel, setSelectedModel] = useState("vendor/one");
  const [personaPickerOpen, setPersonaPickerOpen] = useState(false);
  const [mcpPickerOpen, setMcpPickerOpen] = useState(false);
  const promptRef = useRef<HTMLTextAreaElement | null>(null);
  const modelPickerRef = useRef<HTMLDivElement | null>(null);
  const modelInputRef = useRef<HTMLInputElement | null>(null);
  const personaPickerRef = useRef<HTMLDivElement | null>(null);
  const mcpPickerRef = useRef<HTMLDivElement | null>(null);
  // Mirrors ChatExperience's outside-click close for the tools popover
  // (same ref check), so a test can prove a control INSIDE the popover keeps
  // it open and a mousedown outside closes it.
  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (mcpPickerRef.current?.contains(target)) return;
      setMcpPickerOpen(false);
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => window.removeEventListener("mousedown", onPointerDown);
  }, []);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const dragCounterRef = useRef(0);
  const activeConversationIdRef = useRef<string | null>(null);
  const abortControllersRef = useRef<Map<string, AbortController>>(new Map());
  const noop = () => {};
  const props = {
    prompt,
    setPrompt,
    promptPlaceholder: "Ask anything",
    promptRef,
    submitPrompt: noop,
    sealed: false,
    isStreaming: false,
    isUploadingAttachments: false,
    isDraggingOver: false,
    setIsDraggingOver: noop,
    dragCounterRef,
    fileInputRef,
    addAttachmentFiles: noop,
    pendingAttachments: [],
    attachmentError: null,
    removePendingAttachment: noop,
    uploadSizeWarning: null,
    spreadsheetNudge: { show: false },
    setSpreadsheetNudgeDismissed: noop,
    personas: ["default"],
    selectedPersona: "default",
    setSelectedPersona: noop,
    personaPickerOpen,
    setPersonaPickerOpen,
    personaPickerRef,
    selectedModel,
    setSelectedModel,
    selectedModelLabel: "Model One",
    selectedModelPrices: null,
    modelError: null,
    modelPickerOpen,
    setModelPickerOpen,
    modelPickerRef,
    modelInputRef,
    modelSearchQuery,
    setModelSearchQuery,
    filteredRankedModels: MODELS,
    isLoadingRankedModels: false,
    isLoadingCatalog: false,
    loadRankedModels: noop,
    loadCatalogModels: noop,
    skills: [],
    mcpServers: [],
    mcpPickerOpen,
    setMcpPickerOpen,
    mcpPickerRef,
    isLoadingMcpServers: false,
    loadMcpServerCatalog: noop,
    toggleMcpServer: noop,
    setAllMcpServers: noop,
    setMcpServerAccount: noop,
    activeConversationId: null,
    messages: [],
    contextUsage: null,
    isSummarizing: false,
    compactToastVisible: false,
    setConfirmSummarize: noop,
    activeConversationIdRef,
    abortControllersRef,
    isPendingKey: () => false,
  } as unknown as Parameters<typeof Composer>[0];
  return (
    <>
      <Composer {...props} {...overrides} />
      {/* Mirrors the host's selectedModel so a test can assert what the
          picker committed (or, on a dismiss, left alone). */}
      <span data-testid="host-selected-model">{selectedModel}</span>
    </>
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Composer — model picker focus and Escape", () => {
  it("focuses the search field when the picker opens, and Escape closes it back to the chip", async () => {
    render(<Host />);
    const chip = screen.getByRole("button", { name: /Model One/ });
    expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument();

    fireEvent.click(chip);
    const search = await screen.findByRole("combobox", { name: "Model" });
    // Was autoFocus; now an effect keyed on the picker being open. Same result.
    await waitFor(() => expect(search).toHaveFocus());

    // Escape is delegated to the presentational wrapper so it works from the
    // search field as well as from the chip itself.
    fireEvent.keyDown(search, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument(),
    );
    expect(chip).toHaveFocus();
  });
});

// Multi-login seats in the tools popover (#988): a server with labeled seats
// gets a compact Account select beneath its toggle row. Changing it must reach
// setMcpServerAccount and must NOT flip the row (the select is a sibling of
// the row button and stops propagation), while the row itself still toggles.
describe("Composer — tools popover seat picker", () => {
  it("changing the seat calls setMcpServerAccount without toggling the row", async () => {
    const toggleMcpServer = vi.fn();
    const setMcpServerAccount = vi.fn();
    render(
      <Host
        overrides={{
          mcpServers: [
            {
              name: "gamma",
              display_name: "Gamma",
              description: "Decks and docs.",
              tools: ["create_deck"],
              tool_count: 1,
              enabled: true,
              accounts: ["personal", "work"],
              default_account: "work",
              remote: true,
            },
            {
              name: "solo",
              description: "No seats.",
              tools: [],
              tool_count: 2,
              enabled: false,
            },
          ],
          toggleMcpServer,
          setMcpServerAccount,
        }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    const select = (await screen.findByTestId("mcp-seat-gamma")) as HTMLSelectElement;
    // Only the server with seats gets a picker; the Default option names the
    // default seat.
    expect(screen.queryByTestId("mcp-seat-solo")).not.toBeInTheDocument();
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
      "Default (work)",
      "personal",
      "work",
    ]);

    fireEvent.click(select);
    fireEvent.change(select, { target: { value: "personal" } });
    expect(setMcpServerAccount).toHaveBeenCalledWith(null, "gamma", "personal");
    expect(toggleMcpServer).not.toHaveBeenCalled();

    // The row is still the toggle it always was.
    fireEvent.click(screen.getByRole("button", { name: /Decks and docs/ }));
    expect(toggleMcpServer).toHaveBeenCalledWith(null, "gamma");
  });
});

// "All on" / "All off" in the tools popover: one control for the whole
// optional list, never touching always-on rows, and never offered when there
// is nothing optional to flip. The visible text IS the accessible name
// (WCAG 2.5.3): no aria-label restates it.
describe("Composer — all connectors on / off", () => {
  const ROWS = [
    {
      name: "email",
      display_name: "Email",
      description: "",
      tools: [],
      tool_count: 3,
      enabled: true,
      always_on: true,
    },
    { name: "gamma", description: "", tools: [], tool_count: 2, enabled: true },
    { name: "xandr", description: "", tools: [], tool_count: 5, enabled: false },
  ];
  const allOn = () => screen.getByRole("button", { name: "All on" });
  const allOff = () => screen.getByRole("button", { name: "All off" });

  it("offers both actions, shows the count, and calls setAllMcpServers with the direction", () => {
    const setAllMcpServers = vi.fn();
    render(<Host overrides={{ mcpServers: ROWS, setAllMcpServers }} />);
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    expect(screen.getByTestId("chat-mcp-all-actions")).toHaveTextContent("1 of 2 on");
    expect(allOn()).not.toHaveAttribute("aria-disabled");
    expect(allOff()).not.toHaveAttribute("aria-disabled");
    fireEvent.click(allOn());
    expect(setAllMcpServers).toHaveBeenCalledWith(null, true);
    fireEvent.click(allOff());
    expect(setAllMcpServers).toHaveBeenCalledWith(null, false);
    expect(setAllMcpServers).toHaveBeenCalledTimes(2);
  });

  it("marks the action that would change nothing aria-disabled and ignores its click", () => {
    const setAllMcpServers = vi.fn();
    const everyOn = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: true }));
    const { unmount } = render(<Host overrides={{ mcpServers: everyOn, setAllMcpServers }} />);
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    expect(screen.getByTestId("chat-mcp-all-actions")).toHaveTextContent("2 of 2 on");
    expect(allOn()).toHaveAttribute("aria-disabled", "true");
    expect(allOff()).not.toHaveAttribute("aria-disabled");
    fireEvent.click(allOn());
    expect(setAllMcpServers).not.toHaveBeenCalled();
    unmount();

    const everyOff = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: false }));
    render(<Host overrides={{ mcpServers: everyOff, setAllMcpServers }} />);
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    expect(screen.getByTestId("chat-mcp-all-actions")).toHaveTextContent("0 of 2 on");
    expect(allOn()).not.toHaveAttribute("aria-disabled");
    expect(allOff()).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(allOff());
    expect(setAllMcpServers).not.toHaveBeenCalled();
  });

  it("keeps a no-op action focusable so Escape still closes the popover and returns focus", () => {
    const everyOn = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: true }));
    render(<Host overrides={{ mcpServers: everyOn }} />);
    const trigger = screen.getByRole("button", { name: "Connectors" });
    fireEvent.click(trigger);
    const button = allOn();
    expect(button).toHaveAttribute("aria-disabled", "true");
    button.focus();
    expect(document.activeElement).toBe(button);
    fireEvent.keyDown(button, { key: "Escape" });
    expect(screen.queryByTestId("chat-mcp-all-actions")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it("stays open on a mousedown inside the bar and closes on one outside", () => {
    render(<Host overrides={{ mcpServers: ROWS }} />);
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    fireEvent.mouseDown(allOff());
    expect(screen.getByTestId("chat-mcp-all-actions")).toBeInTheDocument();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByTestId("chat-mcp-all-actions")).not.toBeInTheDocument();
  });

  it("is not offered when every row is always-on", () => {
    render(<Host overrides={{ mcpServers: ROWS.filter((r) => r.always_on) }} />);
    fireEvent.click(screen.getByRole("button", { name: "Connectors" }));
    expect(screen.getByTestId("chat-mcp-always-on-email")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-mcp-all-actions")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "All on" })).not.toBeInTheDocument();
  });
});

describe("Composer — always-on connector status", () => {
  it("shows available and unavailable rows as locked status, with a dimmed available switch", async () => {
    const toggleMcpServer = vi.fn();
    render(
      <Host
        overrides={{
          mcpServers: [
            {
              name: "email",
              display_name: "Email",
              description: "Inbound reports.",
              tools: [],
              tool_count: 10,
              enabled: true,
              always_on: true,
            },
            {
              name: "broken",
              description: "Expected connector.",
              tools: [],
              tool_count: 0,
              enabled: false,
              always_on: true,
            },
            {
              name: "gamma",
              description: "Decks and docs.",
              tools: ["create_deck"],
              tool_count: 1,
              enabled: true,
            },
          ],
          toggleMcpServer,
        }}
      />,
    );

    const trigger = screen.getByRole("button", { name: "Connectors" });
    // Only the selected Optional connector contributes to the badge.
    expect(trigger).toHaveTextContent("1");
    fireEvent.click(trigger);

    const email = await screen.findByTestId("chat-mcp-always-on-email");
    expect(email).toHaveTextContent("Always on");
    expect(email.querySelector('[data-state="always-on"]')).toHaveClass(
      "bg-[var(--color-connector-always-on-track)]",
    );
    const broken = screen.getByTestId("chat-mcp-always-on-broken");
    expect(broken).toHaveTextContent("Unavailable");
    expect(broken.querySelector('[data-state="off"]')).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Inbound reports/ })).not.toBeInTheDocument();
    expect(toggleMcpServer).not.toHaveBeenCalled();
  });
});

// The search field's text is a draft, not the selection. It used to be written
// into selectedModel on every keystroke, so typing "cla" and then pressing
// Escape or clicking away — both of which only close the popover — left "cla"
// as the model and the next send failed. selectedModel changes only on a
// commit (Enter or a row pick); a dismiss leaves the prior selection untouched.
describe("Composer — model search text is a draft until committed", () => {
  const selectedModel = () => screen.getByTestId("host-selected-model").textContent;

  it("does not change the selected model while typing, and Escape leaves it alone", async () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: /Model One/ }));
    const search = await screen.findByRole("combobox", { name: "Model" });
    fireEvent.change(search, { target: { value: "cla" } });
    expect(search).toHaveValue("cla");
    expect(selectedModel()).toBe("vendor/one");

    fireEvent.keyDown(search, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument(),
    );
    expect(selectedModel()).toBe("vendor/one");
  });

  it("commits the highlighted row on Enter", async () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: /Model One/ }));
    const search = await screen.findByRole("combobox", { name: "Model" });
    fireEvent.change(search, { target: { value: "two" } });
    // The host hands in a fixed roster, so ArrowDown moves the highlight to
    // the second row and Enter takes it.
    fireEvent.keyDown(search, { key: "ArrowDown" });
    fireEvent.keyDown(search, { key: "Enter" });
    await waitFor(() => expect(selectedModel()).toBe("vendor/two"));
  });

  it("commits the typed text as a free slug on Enter when no row matches", async () => {
    render(<Host overrides={{ filteredRankedModels: [] }} />);
    fireEvent.click(screen.getByRole("button", { name: /Model One/ }));
    const search = await screen.findByRole("combobox", { name: "Model" });
    fireEvent.change(search, { target: { value: "  custom/slug " } });
    fireEvent.keyDown(search, { key: "Enter" });
    await waitFor(() => expect(selectedModel()).toBe("custom/slug"));
  });
});

// submitPrompt refuses to send while modelError is set, so an enabled Send
// button was a click that did nothing. It must be disabled, and say why.
describe("Composer — Send while the model is rejected", () => {
  it("disables Send and puts the rejection in its title", () => {
    const modelError = {
      message: "This model's completion price exceeds the workspace cap.",
      modelsUrl: "https://openrouter.ai/models",
    };
    render(<Host overrides={{ prompt: "hello", modelError }} />);
    const send = screen.getByRole("button", { name: "Send message" });
    expect(send).toBeDisabled();
    expect(send).toHaveAttribute("title", modelError.message);
  });

  it("keeps Send enabled for the same prompt once the error clears", () => {
    render(<Host overrides={{ prompt: "hello", modelError: null }} />);
    expect(screen.getByRole("button", { name: "Send message" })).toBeEnabled();
  });
});
