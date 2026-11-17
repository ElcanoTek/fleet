// isInteractiveTarget reports whether an element owns Enter itself (a button,
// link, field or ARIA widget). Two things ask, and they must agree, which is
// why this is one function:
//   - Chat's Enter-to-open shortcut declines the key (and so never calls
//     preventDefault) while one of these has focus;
//   - the phone drawer, opened by the j / k shortcut, leaves focus where it is
//     only when that Enter would still work, i.e. when none of these has it.
// When the two used different tests ("is a control" against "is not <body>"),
// focus resting on something that is neither, such as a scrollable region,
// was moved into the drawer and Enter pressed its first button instead of
// opening the chat under the cursor.
export function isInteractiveTarget(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  const tag = el.tagName;
  if (
    tag === "BUTTON" ||
    tag === "A" ||
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT"
  ) {
    return true;
  }
  const role = el.getAttribute("role");
  return (
    role === "button" ||
    role === "link" ||
    role === "menuitem" ||
    role === "option" ||
    role === "tab"
  );
}
