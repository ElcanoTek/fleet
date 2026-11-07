// copyText — the one way the UI writes text to the clipboard (#2001).
//
// navigator.clipboard is missing in an insecure context and in some embedded
// browsers, and writeText rejects when the page lacks focus or the permission
// is denied — a rejected PROMISE, which a try/catch around an un-awaited call
// misses, so call sites kept leaking unhandled rejections or showing "Copied"
// for a copy that never happened. copyText feature-detects, awaits, and never
// rejects: it answers whether the text is now on the clipboard, and a caller
// shows its "Copied" state only on true.
//
// writeText is called synchronously from the caller's event handler (an async
// function runs up to its first await), so the user-gesture requirement some
// browsers place on clipboard writes still holds.

export async function copyText(text: string): Promise<boolean> {
  try {
    const clip = typeof navigator !== "undefined" ? navigator.clipboard : undefined;
    if (!clip || typeof clip.writeText !== "function") return false;
    await clip.writeText(text);
    return true;
  } catch {
    return false;
  }
}
