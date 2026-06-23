"use client";

import { useId, useRef, useState } from "react";
import {
  filterModels,
  loadModels,
  SEED_MODELS,
  type PickerModel,
} from "@/app/shared/lib/models";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";

// ModelPicker — combobox autocomplete over OpenRouter model slugs. React port
// of moc's model-picker.js. The pure filtering/affordability logic lives in
// shared/lib/models.ts (unit-tested separately); this component owns the
// browse/filter modes, keyboard nav, and commit behavior.
//
// Browse mode (focus / after commit): show the full catalog, ignoring the
// current value as a filter — so a populated input still reveals the list.
// Filter mode (after typing): the input value drives ranked filtering. Any
// custom slug can be typed and used even if it isn't in the catalog.

export type ModelPickerProps = {
  id?: string;
  value: string;
  onChange: (slug: string) => void;
  placeholder?: string;
  "aria-describedby"?: string;
};

export function ModelPicker({ id, value, onChange, placeholder, ...rest }: ModelPickerProps) {
  const generatedId = useId();
  const inputId = id ?? generatedId;
  const listboxId = `${inputId}-listbox`;

  const [open, setOpen] = useState(false);
  const [isUserTyping, setIsUserTyping] = useState(false);
  const [activeIndex, setActiveIndex] = useState(-1);
  const inputRef = useRef<HTMLInputElement>(null);

  // The catalog loads lazily the first time the dropdown opens (gated by
  // `enabled: open`). loadModels() is module-level cached and falls back to
  // SEED_MODELS on failure, so reopening is instant and `data` persists across
  // close/reopen. The shared hook owns the cancelled-ref guard that used to
  // live here — and with it, the one-shot load-flag setState-in-effect disable.
  const { data: models, loading } = useCancellableFetch<PickerModel[]>(
    loadModels,
    [],
    { enabled: open },
  );

  const query = isUserTyping ? value : "";
  const source = models ?? SEED_MODELS;
  const visible = filterModels(source, query);

  const commit = (slug: string) => {
    onChange(slug);
    setIsUserTyping(false);
    setOpen(false);
    setActiveIndex(-1);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        setOpen(true);
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setActiveIndex((i) => (i < 0 ? 0 : Math.min(i + 1, visible.length - 1)));
        break;
      case "ArrowUp":
        e.preventDefault();
        setActiveIndex((i) => (i < 0 ? visible.length - 1 : Math.max(i - 1, 0)));
        break;
      case "Enter":
        if (activeIndex >= 0 && visible[activeIndex]) {
          e.preventDefault();
          commit(visible[activeIndex].id);
        }
        break;
      case "Escape":
        e.preventDefault();
        setOpen(false);
        break;
      default:
        break;
    }
  };

  return (
    <div className="model-picker">
      <input
        ref={inputRef}
        id={inputId}
        type="text"
        className="model-picker-input"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={open}
        aria-controls={listboxId}
        autoComplete="off"
        autoCorrect="off"
        autoCapitalize="off"
        spellCheck={false}
        placeholder={placeholder}
        value={value}
        aria-describedby={rest["aria-describedby"]}
        onFocus={() => {
          setIsUserTyping(false);
          setOpen(true);
        }}
        onChange={(e) => {
          setIsUserTyping(true);
          setOpen(true);
          setActiveIndex(-1);
          onChange(e.target.value);
        }}
        onKeyDown={onKeyDown}
        onBlur={() => {
          // Defer so a click on an option commits before close.
          setTimeout(() => setOpen(false), 120);
        }}
      />
      {open ? (
        <div id={listboxId} className="model-picker-dropdown" role="listbox">
          {loading ? (
            <div className="model-picker-loading">Loading models from OpenRouter…</div>
          ) : visible.length === 0 ? (
            <div className="model-picker-empty">
              No matching models — type a custom slug to use it.
            </div>
          ) : (
            visible.map((model, index) => (
              <button
                key={model.id}
                type="button"
                role="option"
                aria-selected={index === activeIndex}
                className={`model-picker-item${index === activeIndex ? " is-active" : ""}`}
                data-model-id={model.id}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => commit(model.id)}
              >
                <span className="model-picker-header">
                  <span className="model-picker-name">{model.name}</span>
                  {model.recommended ? (
                    <span className="model-picker-badge">Recommended</span>
                  ) : null}
                </span>
                <span className="model-picker-slug">{model.id}</span>
              </button>
            ))
          )}
        </div>
      ) : null}
    </div>
  );
}

export default ModelPicker;
