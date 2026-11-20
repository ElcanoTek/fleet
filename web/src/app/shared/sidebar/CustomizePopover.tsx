import { t as tr } from "@/app/lib/i18n";
import { useId, useState } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import { RAIL_ICON_BUTTON_CLASS } from "@/app/shared/ui/railStyles";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";
import { DensityControl, startPageOptions } from "./SidebarEditor";
import { setTeamChatsMode, teamChatsMode, updateItem, updateSection, type SidebarLayout } from "./layout";
import { openSidebarCustomizer, retrySidebarSave, setSidebarLayout, useSidebar } from "./store";
import { usePatrolIds, useSidebarTargets } from "./targets";
import { useCustomizeAccess } from "./useCanCustomize";

// CustomizeButton — the gear in the sidebar's header and the small panel it
// opens (docs/SIDEBAR.md). The panel holds the few choices people change
// most: where fleet opens, how dense the rows are, and which of the standing
// parts show. Everything else is one step away in the full Customize panel
// ("More options"). Each change is the real thing: it shows in the sidebar at
// once and saves to the account a moment later, exactly as in the full panel.

const LABEL = "text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]";

export function CustomizeButton({ collapsed }: { collapsed: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          data-testid="customize-sidebar-button"
          aria-label={tr("Customize sidebar")}
          title={tr("Customize sidebar")}
          // The icon strip holds only the + and the top items: there the
          // gear is not drawn (Customize stays in the account menu). A phone
          // always reports the rail collapsed, so the test is the width's.
          className={cn(RAIL_ICON_BUTTON_CLASS, collapsed && "sm:hidden", open && "bg-[var(--color-overlay-soft)] text-[var(--color-text-primary)]")}
        >
          <Icon name="settings" className="size-[1.05rem]" />
        </button>
      </PopoverTrigger>
      {open ? (
        <PopoverContent
          side="bottom"
          align="start"
          collisionPadding={12}
          data-testid="customize-popover"
          aria-label={tr("Customize sidebar")}
          className="flex max-h-[min(38rem,var(--radix-popover-content-available-height))] w-[20rem] flex-col gap-0 p-0"
        >
          <QuickCustomize
            onMore={() => {
              setOpen(false);
              openSidebarCustomizer();
            }}
          />
        </PopoverContent>
      ) : null}
    </Popover>
  );
}

function QuickCustomize({ onMore }: { onMore: () => void }) {
  const { layout, status, saving, saveError } = useSidebar();
  const { canCustomize, account } = useCustomizeAccess();
  const targets = useSidebarTargets();
  const patrolIds = usePatrolIds(true);
  const homeLabel = useId();
  const change = (next: SidebarLayout) => {
    if (canCustomize) setSidebarLayout(next);
  };

  const options = startPageOptions(layout, (item) => targets?.titleFor(item), patrolIds, targets?.pins);
  const current = options.find((o) => o.selected) ?? options[0];
  const item = (id: string) => layout.items.find((i) => i.id === id);
  const section = (id: "projects") => layout.sections.find((s) => s.id === id);

  const shows: { key: string; label: string; on: boolean; set: (on: boolean) => SidebarLayout }[] = [
    { key: "patrols", label: tr("Patrols"), on: !item("patrols")?.hidden, set: (on) => updateItem(layout, "patrols", { hidden: !on }) },
    { key: "tasks", label: tr("Background tasks"), on: !item("tasks")?.hidden, set: (on) => updateItem(layout, "tasks", { hidden: !on }) },
    { key: "projects", label: tr("Projects"), on: !section("projects")?.hidden, set: (on) => updateSection(layout, "projects", { hidden: !on }) },
    {
      key: "team",
      label: tr("Teammates' shared chats"),
      on: teamChatsMode(layout) !== "hidden",
      // Back on returns them to where they were: a list of their own stays one.
      set: (on) => setTeamChatsMode(layout, on ? (layout.separate_lists ? "own" : "chats") : "hidden"),
    },
  ];

  return (
    <>
      <div className="flex items-center gap-2 px-4 pb-1 pt-3.5">
        <h2 className="min-w-0 flex-1 text-[0.95rem] font-semibold text-[var(--color-text-primary)]">{tr("Customize sidebar")}</h2>
      </div>
      {/* The scroller is a div: a fieldset does not scroll its own content. */}
      <div className="min-h-0 flex-1 overflow-y-auto">
      <fieldset disabled={!canCustomize} className="m-0 grid min-w-0 gap-4 border-0 px-4 pb-3 pt-2">
        <section className="grid gap-1.5">
          <h3 id={homeLabel} className={LABEL}>
            {tr("Fleet opens to")}
          </h3>
          <RadioGroup
            aria-labelledby={homeLabel}
            data-testid="customize-home"
            value={current.key}
            onValueChange={(key) => {
              const picked = options.find((o) => o.key === key);
              if (picked) change(picked.apply(layout));
            }}
            className="-mx-1.5 max-h-[13.5rem] gap-0 overflow-y-auto"
          >
            {options.map((o) => {
              const id = `${homeLabel}-${o.key || "last"}`;
              return (
                <label
                  key={o.key}
                  htmlFor={id}
                  data-testid="customize-home-option"
                  data-selected={o.selected ? "true" : undefined}
                  className={cn(
                    "flex min-h-8 cursor-pointer items-center gap-2.5 rounded-md px-1.5 py-1 text-[0.84rem] transition hover:bg-[var(--color-overlay-soft)] any-pointer-coarse:min-h-10",
                    o.selected ? "text-[var(--color-text-primary)]" : "text-[var(--color-text-secondary)]",
                  )}
                >
                  <RadioGroupItem id={id} value={o.key} />
                  <span className="min-w-0 flex-1 truncate">{o.label}</span>
                  {o.detail ? <span className="shrink-0 text-[0.72rem] text-[var(--color-text-muted)]">{o.detail}</span> : null}
                </label>
              );
            })}
          </RadioGroup>
          <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">
            {tr("Any chat, dashboard or project can be the start page: ⋯ › Set as start page.")}
          </p>
        </section>

        <section className="grid gap-1.5">
          <h3 className={LABEL}>{tr("Density")}</h3>
          <DensityControl layout={layout} onChange={change} />
        </section>

        <section className="grid gap-0.5">
          <h3 className={cn(LABEL, "mb-1")}>{tr("Show")}</h3>
          {shows.map((s) => {
            const id = `${homeLabel}-show-${s.key}`;
            return (
              <div key={s.key} className="flex min-h-8 items-center justify-between gap-3 any-pointer-coarse:min-h-10">
                <label htmlFor={id} className="min-w-0 flex-1 cursor-pointer text-[0.84rem] text-[var(--color-text-primary)]">
                  {s.label}
                </label>
                <Switch id={id} data-testid={`customize-show-${s.key}`} checked={s.on} onCheckedChange={(on) => change(s.set(on))} />
              </div>
            );
          })}
        </section>
      </fieldset>
      </div>
      <div className="flex items-center gap-3 border-t border-[var(--color-border)] px-4 py-2.5">
        <p className="min-w-0 flex-1 text-[0.72rem] leading-snug text-[var(--color-text-muted)]" data-testid="customize-save-state">
          {status === "loading" ? (
            tr("Loading your sidebar…")
          ) : status === "unavailable" ? (
            <span role="alert">{tr("Your sidebar couldn't be loaded, so it can't be changed right now.")}</span>
          ) : account === "viewer" ? (
            tr("Your account is read-only, so your sidebar can't be changed.")
          ) : account === "failed" ? (
            <span role="alert">{tr("Couldn't check your account, so your sidebar can't be changed right now.")}</span>
          ) : saveError ? (
            <>
              <span role="alert" className="text-[var(--color-danger)]">
                {tr("Couldn't save your sidebar.")}
              </span>{" "}
              <button type="button" onClick={retrySidebarSave} className="rounded font-medium text-[var(--color-text-primary)] underline underline-offset-2">
                {tr("Try again")}
              </button>
            </>
          ) : saving ? (
            tr("Saving…")
          ) : (
            tr("Saved to your account, on every device.")
          )}
        </p>
        <button
          type="button"
          data-testid="customize-more"
          onClick={onMore}
          className="inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-1 text-[0.8rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          {tr("More options")}
          <Icon name="arrow-right" className="size-3.5" />
        </button>
      </div>
    </>
  );
}
