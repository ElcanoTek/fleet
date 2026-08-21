import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useRef, useState } from "react";
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
function Host() {
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
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const dragCounterRef = useRef(0);
  const activeConversationIdRef = useRef<string | null>(null);
  const abortControllersRef = useRef<Record<string, AbortController>>({});
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
  return <Composer {...props} />;
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
