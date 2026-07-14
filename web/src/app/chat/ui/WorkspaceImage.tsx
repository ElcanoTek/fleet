"use client";

// WorkspaceImage renders an <img> from the chat workspace with the
// settings that keep it from flickering when the user scrolls.
//
// Lives in its own module (moved verbatim from AssistantContent.tsx) so
// ToolChips can import it WITHOUT statically pulling the whole
// ReactMarkdown pipeline into the initial /chat bundle — AssistantContent
// is lazy-loaded now (see ChatTranscript), and a static
// ToolChips → AssistantContent edge would have defeated that split.
//
// Three fixes layered together:
//   - React.memo: parent re-renders triggered by scroll (the
//     showJumpToLatest visibility update fires on every scroll tick,
//     so without memoization every tick reconciles a fresh <img>
//     tree and mobile browsers blank the paint for a frame).
//   - loading="eager": once the agent shows the user a chart it's
//     intentional content, not a long-article tail. Lazy loading
//     plus aggressive mobile-browser memory unloads on scroll-away
//     was the biggest source of flicker — re-entering viewport
//     would re-fetch and re-decode.
//   - decoding="async": lets the browser decode off the main thread
//     so the scroll keeps its frame budget while the image paints.

import { memo, useState } from "react";

export const WorkspaceImage = memo(function WorkspaceImage({
  src,
  alt,
  title,
}: {
  src: string;
  alt: string;
  title?: string;
}) {
  const [errored, setErrored] = useState(false);
  if (errored) {
    return (
      <span className="my-2 inline-block rounded-md border border-dashed border-[var(--color-border-strong)] px-2 py-1 text-[0.72rem] text-[var(--color-text-muted)]">
        couldn&rsquo;t load image: {alt || src}
      </span>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt={alt}
      title={title}
      loading="eager"
      decoding="async"
      className="my-2 block max-w-full rounded-[0.5rem] border border-[var(--color-border)]"
      onError={() => setErrored(true)}
    />
  );
});
