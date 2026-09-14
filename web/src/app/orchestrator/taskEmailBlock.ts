import { isValidEmail } from "@/app/shared/lib/format";

// The task form's **Email results** recipients are not a column — they are
// delivered as a CRITICAL ACTION block appended to the task's prompt, which the
// agent reads as an instruction to mail its report when it finishes.
//
// That storage choice is load-bearing for a bug this module exists to close.
// The form used to append the block on save and open an edit with an empty
// recipient list, so the routine both user guides recommend — fix the library
// prompt, re-select it on the task, save — replaced the whole prompt, block and
// all, and produced a task that ran correctly and emailed its report to nobody.
// No warning, no diff a reviewer would notice, and the failure is invisible
// until someone asks why the morning report stopped arriving.
//
// So the block round-trips: build it on the way out, split it back off on the
// way in, and the recipients live in the form's own state where replacing the
// prompt cannot touch them.
//
// Parsing is deliberately conservative. Prompts are user-authored text and a
// greedy matcher that ate a paragraph someone wrote by hand would be a worse
// bug than the one being fixed, so a block is only recognized when it matches
// the exact shape buildPromptWithRecipients emits, sits at the very end, and
// carries at least one address that actually parses as one. Anything else is
// left in the prompt verbatim, visible to its author.

const BLOCK_HEADER = "---\nCRITICAL ACTION\nemail:";

/** The instruction block for `recipients`, exactly as the agent expects it. */
function recipientsBlock(recipients: string[]): string {
  const yaml = recipients.map((e) => `    - ${e}`).join("\n");
  return [
    "---",
    "CRITICAL ACTION",
    "email:",
    "  action: send_report",
    "  tool: email",
    '  instruction: "The following action is MANDATORY after completing the core task."',
    '  description: "Send the full report and findings to the listed recipients."',
    "  recipients:",
    yaml,
    "---",
  ].join("\n");
}

/**
 * The prompt as stored on the task: the author's text, plus the delivery
 * instruction when there is anyone to deliver to. No recipients means no block,
 * so a task that never had one is byte-identical to what the author typed.
 */
export function buildPromptWithRecipients(basePrompt: string, recipients: string[]): string {
  const base = basePrompt.trim();
  if (recipients.length === 0) return base;
  return `${base}\n\n${recipientsBlock(recipients)}`;
}

/**
 * The inverse: split a stored prompt back into the author's text and the
 * recipients the form should show. A prompt with no recognizable block comes
 * back unchanged with no recipients — which is also what happens to a block
 * that has been hand-edited into a shape this cannot vouch for.
 */
export function splitPromptRecipients(stored: string): {
  basePrompt: string;
  recipients: string[];
} {
  const text = stored.trimEnd();
  const start = text.lastIndexOf(`\n${BLOCK_HEADER}`);
  if (start === -1) return { basePrompt: stored, recipients: [] };

  const block = text.slice(start + 1);
  if (!block.endsWith("\n---")) return { basePrompt: stored, recipients: [] };

  const recipients = parseRecipients(block);
  // A block whose recipient list we cannot read is not ours to remove: leaving
  // it in the textarea shows the author exactly what is on their task.
  if (recipients.length === 0) return { basePrompt: stored, recipients: [] };

  // Rebuilding from the parsed recipients must reproduce the block byte for
  // byte. That equality is the whole safety argument — it means the only text
  // being taken out of the prompt is text this module would have put there, so
  // nothing an author wrote can be silently absorbed.
  if (block !== recipientsBlock(recipients)) return { basePrompt: stored, recipients: [] };

  return { basePrompt: text.slice(0, start).trimEnd(), recipients };
}

// parseRecipients reads the `    - address` lines under `  recipients:`.
// Anything that is not a valid address disqualifies the whole block rather than
// being dropped, so a partially-understood list never becomes a silently
// shortened one.
function parseRecipients(block: string): string[] {
  const lines = block.split("\n");
  const marker = lines.indexOf("  recipients:");
  if (marker === -1) return [];

  const out: string[] = [];
  for (const line of lines.slice(marker + 1)) {
    if (line === "---") break;
    if (!line.startsWith("    - ")) return [];
    const address = line.slice("    - ".length);
    if (!isValidEmail(address)) return [];
    out.push(address);
  }
  return out;
}
