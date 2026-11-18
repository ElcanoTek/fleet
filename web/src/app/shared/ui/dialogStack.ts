// dialogStack — shared open-dialog stack across DialogShell and useDialogA11y.
//
// In-chat popovers/dialogs (such as PatrolsDialog via useDialogA11y) and
// DialogShell modals (such as AutonomousRiskModal, ApprovalModesInfoModal, and
// PatrolLaunchpad) both install document-level Escape and Tab listeners. Sharing
// one stack ensures that when a confirmation or info modal opens on top of
// either primitive, only the topmost layer handles Escape and Tab.

const stack: unknown[] = [];

export function pushDialogLayer(token: unknown): () => void {
  stack.push(token);
  return () => {
    const idx = stack.indexOf(token);
    if (idx !== -1) stack.splice(idx, 1);
  };
}

export function isTopDialogLayer(token: unknown): boolean {
  return stack.length > 0 && stack[stack.length - 1] === token;
}
