// The guide Markdown is bundled at build time (Vite `?raw` imports). The copies
// under ./guides are kept in sync with the fleet-guide skill's by
// `make sync-guides`, and scripts/check_guides_sync_test.go fails on drift.
import chatGuide from "./guides/chat.md?raw";
import operationsCenterGuide from "./guides/operations-center.md?raw";

/** A guide as the /help surface presents it. */
export type Guide = {
  /** URL segment: /help/<slug>. */
  slug: string;
  /** Sub-nav + card title. */
  title: string;
  /** One line under the title on the index — what this guide answers. */
  blurb: string;
  /** Markdown file inside guides/. */
  file: string;
};

// Ordered as a reader should meet them: chat is where work is built, the
// Operations Center is where it runs.
export const GUIDES: readonly Guide[] = [
  {
    slug: "chat",
    title: "Chat",
    blurb:
      "Asking well, getting data in, approval cards, memory and projects, the prompt library, and turning a conversation into something that runs on its own.",
    file: "chat.md",
  },
  {
    slug: "operations-center",
    title: "Operations Center",
    blurb:
      "Reading the board, what a task is made of, what every run state means, and what to do when a run needs you.",
    file: "operations-center.md",
  },
] as const;

export function findGuide(slug: string): Guide | undefined {
  return GUIDES.find((g) => g.slug === slug);
}

const CONTENT: Record<string, string> = {
  "chat.md": chatGuide,
  "operations-center.md": operationsCenterGuide,
};

/**
 * One guide's Markdown. The filename comes from the GUIDES table above — never
 * from the request — so a bad URL segment is a not-found from findGuide.
 */
export function readGuide(guide: Guide): string {
  return CONTENT[guide.file] ?? "";
}
