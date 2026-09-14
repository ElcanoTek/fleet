import { readFileSync } from "node:fs";
import { join } from "node:path";

// The in-app user guides (/help), rendered from the Markdown in guides/.
//
// SOURCE OF TRUTH: internal/clientconfig/builtin_skills/fleet-guide/*.md — the
// same files the built-in `fleet-guide` skill gives the agent, so the page a
// user reads and the text the assistant quotes back when they ask "how do I
// schedule this?" are one document. guides/ here is a verbatim copy of those
// two files. Edit the skill's copy and run `make sync-guides`;
// scripts/check_guides_sync_test.go fails the build on any drift, with that
// instruction.
//
// The copy exists because neither direction of linking works: go:embed cannot
// reach outside its package, and Turbopack refuses a symlink that leaves the
// Next project root ("points out of the filesystem root") the moment a server
// module reads through it.
//
// Why a file read rather than an import: Next has no loader for .md, and a
// .ts module holding the prose would be a third copy of it. The root layout is
// force-dynamic (white-labeling reads the bundle per request), so these pages
// render per request too — hence the module-level cache below: a guide is read
// from disk once per server process, not once per reader.

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

const GUIDES_DIR = join(process.cwd(), "src", "app", "help", "guides");

const cache = new Map<string, string>();

/**
 * Read one guide's Markdown, memoized for the life of the process. The
 * filename comes from the GUIDES table above — never from the request — so a
 * bad URL segment is a 404 from findGuide, not a path this function ever sees.
 */
export function readGuide(guide: Guide): string {
  const cached = cache.get(guide.file);
  if (cached !== undefined) return cached;
  const content = readFileSync(join(GUIDES_DIR, guide.file), "utf8");
  cache.set(guide.file, content);
  return content;
}
