"use client";

import { useEffect, useState } from "react";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { validateConcurrencyCap } from "@/app/shared/lib/validation";
import { useToast } from "@/app/shared/ui/Toast";

// ConcurrencyCapSetting — the single global concurrency-cap control
// (FLEET_MAX_CONCURRENT_AGENTS). New in v2: one configurable cap bounds
// simultaneous agents across interactive + scheduled. Reads GET /concurrency,
// writes PUT /concurrency. Validated by validateConcurrencyCap before save.

export type ConcurrencyCapSettingProps = {
  // Optional seed (e.g. from a parent that already loaded it); otherwise the
  // component fetches on mount.
  initialValue?: number;
};

export function ConcurrencyCapSetting({ initialValue }: ConcurrencyCapSettingProps) {
  const { showToast } = useToast();
  const [value, setValue] = useState<string>(initialValue != null ? String(initialValue) : "");
  const [warmPool, setWarmPool] = useState<number | undefined>(undefined);
  const [error, setError] = useState<string>("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (initialValue != null) return;
    let cancelled = false;
    orchestratorApi
      .concurrency()
      .then((cfg) => {
        if (cancelled) return;
        setValue(String(cfg.max_concurrent_agents));
        setWarmPool(cfg.warm_pool_size);
      })
      .catch(() => {
        /* leave blank → server default */
      });
    return () => {
      cancelled = true;
    };
  }, [initialValue]);

  const save = async () => {
    const v = validateConcurrencyCap(value);
    if (!v.valid) {
      setError(v.message);
      return;
    }
    setError("");
    const parsed = Number.parseInt(value, 10);
    setSaving(true);
    try {
      const cfg = await orchestratorApi.setConcurrency(parsed);
      setValue(String(cfg.max_concurrent_agents));
      setWarmPool(cfg.warm_pool_size);
      showToast("Concurrency cap updated", "success");
    } catch (err) {
      showToast(`Failed to update cap: ${(err as Error).message}`, "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="form-group advanced-section-group" data-testid="concurrency-cap-setting">
      <div className="form-label-row">
        <label htmlFor="concurrencyCapInput">Global Concurrency Cap</label>
      </div>
      <div className="concurrency-cap-row">
        <input
          id="concurrencyCapInput"
          type="number"
          min={1}
          max={64}
          step={1}
          placeholder="4 (default)"
          aria-describedby="concurrencyCapHelp"
          aria-invalid={error ? "true" : undefined}
          value={value}
          onChange={(e) => {
            setValue(e.target.value);
            if (error) setError("");
          }}
        />
        <button type="button" className="btn btn-secondary" disabled={saving} onClick={() => void save()}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
      <div id="concurrencyCapHelp" className="advanced-setting-meta">
        Max simultaneous agents across interactive chat + scheduled tasks
        (FLEET_MAX_CONCURRENT_AGENTS).
        {warmPool != null ? ` Warm pool: ${warmPool}.` : null}
      </div>
      {error ? (
        <div className="validation-error" aria-live="polite" data-testid="concurrency-cap-error">
          {error}
        </div>
      ) : null}
    </div>
  );
}

export default ConcurrencyCapSetting;
