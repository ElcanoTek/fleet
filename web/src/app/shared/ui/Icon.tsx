// Icon renders a symbol from the shared core-icons sprite. Both the chat view
// and the login card hand-rolled this same one-liner; it now lives here so the
// shared shell components (ThemeToggle, …) reference a single definition.
export function Icon({ name, className }: { name: string; className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
      <use href={`/icons/core-icons.svg#${name}`} />
    </svg>
  );
}

export default Icon;
