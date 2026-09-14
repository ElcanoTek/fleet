// Heading extraction + slugging for the guide pages.
//
// The guides are ordinary Markdown with in-document links written the way every
// Markdown host renders them ("see [Run states](#4-run-states)"). That only
// works if the ids we put on the rendered headings are the ones those links
// expect, so slugify() implements the same rule the guides were written
// against: lowercase, drop everything that is not a letter, digit, space or
// hyphen, then collapse whitespace runs to single hyphens. The same function
// feeds the on-page contents list, which is why the two can never disagree.

/** One `##`/`###` heading of a guide, in document order. */
export type Heading = {
  /** Anchor id, also the fragment an in-guide link points at. */
  id: string;
  /** Heading text as written. */
  text: string;
  /** 2 for a section, 3 for a subsection. */
  depth: 2 | 3;
};

export function slugify(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9 -]/g, "")
    .trim()
    .replace(/\s+/g, "-");
}

// Fenced code blocks are skipped: a "## " line inside one is code, not a
// heading, and fabricating an anchor for it would put a phantom row in the
// contents list.
export function extractHeadings(markdown: string): Heading[] {
  const headings: Heading[] = [];
  let inFence = false;
  for (const line of markdown.split("\n")) {
    if (line.startsWith("```")) {
      inFence = !inFence;
      continue;
    }
    if (inFence) continue;
    const match = /^(#{2,3}) +(.+?)\s*$/.exec(line);
    if (!match) continue;
    const text = match[2];
    headings.push({ id: slugify(text), text, depth: match[1].length as 2 | 3 });
  }
  return headings;
}
