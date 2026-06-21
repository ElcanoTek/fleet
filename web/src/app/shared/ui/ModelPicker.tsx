"use client";

import { useEffect, useId, useRef, useState } from "react";
import {
  filterModels,
  loadModels,
  SEED_MODELS,
  type PickerModel,
} from "@/app/shared/lib/models";

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
  const [models, setModels] = useState<PickerModel[] | null>(null);
  const [isUserTyping, setIsUserTyping] = useState(false);
  const [activeIndex, setActiveIndex] = useState(-1);
  const [loading, setLoading] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const query = isUserTyping ? value : "";
  const source = models ?? SEED_MODELS;
  const visible = filterModels(source, query);

  useEffect(() => {
    if (!open || models) return;
    let cancelled = false;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot load flag
    setLoading(true);
    loadModels()
      .then((m) => {
        if (!cancelled) setModels(m);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, models]);

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
