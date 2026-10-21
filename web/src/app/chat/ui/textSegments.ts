/**
 * joinTextSegments joins two separately written pieces of assistant text
 * (two model steps, two reasoning blocks, two persisted text entries) as two
 * paragraphs. Newlines already at the seam count toward the break, so text
 * that ends with a blank line gets nothing extra; an empty side is returned
 * untouched. Mirrors agentcore.StepTextSeparator.
 */
export function joinTextSegments(prev: string, next: string): string {
  if (!prev.trim()) return next;
  if (!next) return prev;
  const trailing = prev.slice(prev.trimEnd().length);
  const leading = next.slice(0, next.length - next.trimStart().length);
  const have = (trailing.match(/\n/g)?.length ?? 0) + (leading.match(/\n/g)?.length ?? 0);
  return prev + "\n".repeat(Math.max(0, 2 - have)) + next;
}
