import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DownloadChatDialog } from "./DownloadChatDialog";

// The download chooser that replaced the "Download as JSON" menu item. The
// point of the change is that the DEFAULT is the file a non-technical reader
// can use, and that each option is described by what it is for — so these
// tests pin the default, the plain-language options, and the scope switch.

describe("DownloadChatDialog", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  const setup = (onDownload = vi.fn().mockResolvedValue(undefined)) => {
    const onClose = vi.fn();
    render(
      <DownloadChatDialog
        conversationTitle="Nissan CPA forecast"
        onDownload={onDownload}
        onClose={onClose}
      />,
    );
    return { onDownload, onClose };
  };

  it("offers each format in plain language, not by file extension", () => {
    setup();
    expect(screen.getByText("Web page")).toBeInTheDocument();
    expect(screen.getByText("Text document")).toBeInTheDocument();
    expect(screen.getByText("Raw data")).toBeInTheDocument();
    // The old label named a format the reader had to already understand.
    expect(screen.queryByText(/download as json/i)).not.toBeInTheDocument();
  });

  it("defaults to the readable web page without the agent's working trail", async () => {
    const { onDownload, onClose } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() =>
      expect(onDownload).toHaveBeenCalledWith({
        format: "html",
        includeWork: false,
      }),
    );
    // The browser's own Save dialog takes over, so ours gets out of the way.
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("carries the working trail when the user asks for it", async () => {
    const { onDownload } = setup();
    fireEvent.click(screen.getByLabelText(/include the agent's work/i));
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() =>
      expect(onDownload).toHaveBeenCalledWith({
        format: "html",
        includeWork: true,
      }),
    );
  });

  it("hides the scope choice for raw data, which always carries everything", () => {
    setup();
    expect(screen.getByLabelText(/include the agent's work/i)).toBeVisible();
    fireEvent.click(screen.getByRole("radio", { name: /raw data/i }));
    expect(
      screen.queryByLabelText(/include the agent's work/i),
    ).not.toBeInTheDocument();
  });

  it("picks the markdown document when chosen", async () => {
    const { onDownload } = setup();
    fireEvent.click(screen.getByRole("radio", { name: /text document/i }));
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() =>
      expect(onDownload).toHaveBeenCalledWith({
        format: "markdown",
        includeWork: false,
      }),
    );
  });

  it("keeps the dialog open and explains a failed download", async () => {
    const onDownload = vi.fn().mockRejectedValue(new Error("chat server down"));
    const { onClose } = setup(onDownload);
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(await screen.findByText("chat server down")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    // Still retryable, not stuck in the busy state.
    expect(screen.getByRole("button", { name: "Download" })).toBeEnabled();
  });
});
