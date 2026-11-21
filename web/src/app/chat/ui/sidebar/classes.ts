// Class strings the sidebar's rows and menus share (ConversationSidebar and
// the modules beside this one), and the drag payload's type for filing a
// chat into a project.

// A row's options (⋯) button: revealed on hover or keyboard focus where
// there is a mouse, always shown on a touch screen, which has no hover to
// reveal it with (a phone user otherwise had to know where to tap).
export const ROW_ACTIONS_REVEAL =
  "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 any-pointer-coarse:opacity-100";

// While the ⋯ button shows, the row's status steps aside for it and the title
// keeps clear of it. A mouse reveals the button on hover or focus (and while
// its menu is open); a touch screen always shows it, so there the status
// stays, to its left.
export const ROW_CLEAR_FOR_ACTIONS =
  "group-hover:pr-9 group-focus-within:pr-9 group-has-[[aria-expanded=true]]:pr-9 any-pointer-coarse:pr-11";
export const STATUS_YIELDS_TO_ACTIONS =
  "group-hover:hidden group-focus-within:hidden group-has-[[aria-expanded=true]]:hidden any-pointer-coarse:!inline-flex";

export const KEBAB_CLASS =
  // A fixed, centered, padded square: a rounded hover highlight around the
  // centered icon. hit-area extends the clickable box without growing the fill.
  "hit-area hit-area-touch pointer-events-auto inline-flex size-7 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none";

export const RENAME_INPUT_CLASS =
  "mx-1 my-[0.2rem] w-[calc(100%-0.5rem)] rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1 text-[length:var(--rail-row-text)] text-[var(--color-text-primary)] outline-none";

// Dragging a chat onto a project files it there (the drag payload's type).
export const CONV_DRAG_MIME = "application/x-fleet-conversation";
