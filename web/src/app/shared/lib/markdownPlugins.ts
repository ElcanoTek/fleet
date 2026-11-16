import remarkGfm from "remark-gfm";

// The remark plugins for Markdown that a model (or a person in a chat) wrote:
// replies and your own messages, run logs, generated cards. One list, so the
// same text reads the same wherever it is shown. When each renderer kept its
// own, a table that broke its cells' lines in the chat showed the "<br>" tags
// in the task's run log and in a card.

// A literal <br> and what is done with it. Raw HTML is shown as text here
// (no rehype-raw, on purpose), and that includes <br> — with ONE exception:
//
//   inside a table cell, a <br> that has content before it and content after
//   it in that cell is a line break.
//
// A table cell is the only place Markdown gives a writer no other way to
// break a line, so a tag standing between two things there can only be
// formatting ("5 seats<br>Email support"), and GitHub-flavoured Markdown
// renders it that way everywhere. A tag with nothing on one side of it does
// not separate anything, so it is not a break: a cell that is only the tag,
// or starts or ends with it ("<br> or <br/>", "Use <br>"), is a table ABOUT
// the tag and keeps it.
//
// Everywhere else the tag is shown exactly as typed, and that is deliberate.
// Outside a table the same characters mean two things — a break the writer
// wants ("Acme Corp<br>") or the tag being talked about ("<p>Hello<br>World
// </p>", "How do I use the <br> tag?") — and nothing in the text settles
// which. Three attempts to guess (every tag; only at a line ending; by the
// spacing around it) each hid something a person had typed. A tag left
// showing costs a stray "<br>" at the end of a line that already breaks
// there (paragraphs keep their line endings); a tag wrongly hidden changes
// what someone wrote. So no guess is made. The tables of cases in
// chat-experience.markdown.test.tsx pin this: change the rule only with a
// row there for every case it moves.
type MdNode = { type: string; value?: string; children?: MdNode[] };
const BR_TAG = /^<br\s*\/?>$/i;
const isBrTag = (node: MdNode): boolean =>
  node.type === "html" && typeof node.value === "string" && BR_TAG.test(node.value.trim());
// Anything a reader would see that is not a <br> tag or blank space.
const isContent = (node: MdNode): boolean =>
  !isBrTag(node) && (node.type !== "text" || (node.value ?? "").trim() !== "");
// The cell's leaves in reading order (inline code, text, tags; emphasis and
// links are descended into).
function leavesOf(node: MdNode, out: MdNode[] = []): MdNode[] {
  if (node.children && node.children.length > 0) for (const child of node.children) leavesOf(child, out);
  else out.push(node);
  return out;
}
export function remarkLineBreakTags() {
  const breakTagsIn = (cell: MdNode) => {
    const leaves = leavesOf(cell);
    let first = 0;
    while (first < leaves.length && !isContent(leaves[first])) first++;
    let last = leaves.length - 1;
    while (last >= 0 && !isContent(leaves[last])) last--;
    for (let i = first + 1; i < last; i++) {
      const leaf = leaves[i];
      if (isBrTag(leaf)) {
        leaf.type = "break";
        delete leaf.value;
      }
    }
  };
  const walk = (node: MdNode) => {
    for (const child of node.children ?? []) {
      if (child.type === "tableCell") breakTagsIn(child);
      else walk(child);
    }
  };
  return (tree: MdNode) => walk(tree);
}

// Hoisted: ReactMarkdown re-parses when it is handed a new plugins array.
export const MODEL_MARKDOWN_PLUGINS = [remarkGfm, remarkLineBreakTags];
