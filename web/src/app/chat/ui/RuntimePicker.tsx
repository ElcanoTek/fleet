"use client";

import { useState } from "react";

// RuntimeFlavor is one selectable runtime flavor, as delivered by
// /api/client-config (the bundle's runtimes catalog). The picker is purely
// driven by this data — no flavor names are hardcoded.
export type RuntimeFlavor = {
  name: string;
  display_name: string;
  description?: string;
  beta?: boolean;
};

// RuntimePicker is the chat flavor picker: it lets a conversation choose which
// runtime flavor backs its turns (native-inprocess fast path vs native-acp
// sandboxed agent, plus future external ACP flavors). It mirrors the persona
// picker's compact dropdown shape.
//
// It self-hides when there are fewer than two flavors, so a bundle that ships
// only the in-process flavor never shows an empty control. The selected flavor
// falls back to defaultRuntime when the conversation has no explicit choice.
export function RuntimePicker({
  flavors,
  selected,
  defaultRuntime,
  onSelect,
  disabled,
}: {
  flavors: RuntimeFlavor[];
  selected: string;
  defaultRuntime: string;
  onSelect: (name: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);

  // A single-flavor (or empty) bundle has nothing to pick — hide the control.
  if (flavors.length < 2) return null;

  const effective = selected || defaultRuntime || flavors[0]?.name || "";
  const current = flavors.find((f) => f.name === effective) ?? flavors[0];
  const label = current ? current.display_name : "Runtime";

  return (
    <div className="relative" data-testid="runtime-picker">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label="Runtime"
        title={`Runtime — ${label}`}
        disabled={disabled}
        data-testid="runtime-picker-button"
        className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-slate-600 hover:bg-slate-100 disabled:opacity-50"
        onClick={() => setOpen((o) => !o)}
      >
        <span className="truncate">{label}</span>
        {current?.beta ? <span className="text-[10px] text-amber-600">beta</span> : null}
      </button>
      {open ? (
        <ul
          role="listbox"
          aria-label="Runtime"
          data-testid="runtime-picker-menu"
          className="absolute bottom-full z-20 mb-1 w-56 rounded-md border border-slate-200 bg-white py-1 shadow-lg"
        >
          {flavors.map((f) => {
            const isSelected = f.name === effective;
            return (
              <li key={f.name}>
                <button
                  type="button"
                  role="option"
                  aria-selected={isSelected}
                  data-testid={`runtime-option-${f.name}`}
                  className={`flex w-full flex-col items-start px-3 py-1.5 text-left text-xs hover:bg-slate-50 ${
                    isSelected ? "bg-slate-50 font-medium" : ""
                  }`}
                  onClick={() => {
                    onSelect(f.name);
                    setOpen(false);
                  }}
                >
                  <span className="flex items-center gap-1">
                    {f.display_name}
                    {f.beta ? <span className="text-[10px] text-amber-600">beta</span> : null}
                  </span>
                  {f.description ? (
                    <span className="text-[10px] text-slate-400">{f.description}</span>
                  ) : null}
                </button>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}
