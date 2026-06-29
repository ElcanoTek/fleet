package models

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
)

func slaIntPtr(i int) *int { return &i }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// roundTripDefinition mirrors the production export→import path for one task:
// TaskToExportRecord → envelope marshal/unmarshal (json or yaml) →
// ExportRecordToTaskCreate → NewTask. It returns the record that crossed the
// wire (to assert what was serialized) and the recreated task.
func roundTripDefinition(
	t *testing.T,
	src *Task,
	marshal func(any) ([]byte, error),
	unmarshal func([]byte, any) error,
) (TaskExportRecord, *Task) {
	t.Helper()
	env := TaskExportEnvelope{Version: TaskExportVersion, Tasks: []TaskExportRecord{TaskToExportRecord(src)}}
	data, err := marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TaskExportEnvelope
	if err := unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("expected 1 task after round-trip, got %d", len(got.Tasks))
	}
	rec := got.Tasks[0]
	return rec, NewTask(ExportRecordToTaskCreate(rec))
}

// TestExportImport_SLARoundTrip proves the follow-up to #274/#238: SLA config
// (expected duration + multipliers) is part of the portable task definition and
// survives an export→import round-trip in both JSON and YAML, while runtime SLA
// state (sla_breached / actual_duration_seconds) never crosses the definition
// boundary.
func TestExportImport_SLARoundTrip(t *testing.T) {
	formats := []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"json", json.Marshal, json.Unmarshal},
		{"yaml", yaml.Marshal, yaml.Unmarshal},
	}

	cases := []struct {
		name         string
		expected     *int
		warn, fail   float64
		wantExpected *int
		wantWarn     float64
		wantFail     float64
		wantOmitted  bool // export record should carry NO SLA config
	}{
		{
			name: "custom multipliers", expected: slaIntPtr(30), warn: 1.2, fail: 1.8,
			wantExpected: slaIntPtr(30), wantWarn: 1.2, wantFail: 1.8,
		},
		{
			name: "default multipliers", expected: slaIntPtr(15), warn: DefaultSLAWarnMultiplier, fail: DefaultSLAFailMultiplier,
			wantExpected: slaIntPtr(15), wantWarn: DefaultSLAWarnMultiplier, wantFail: DefaultSLAFailMultiplier,
		},
		{
			// No expectation = no SLA. The DB-default multipliers (1.5/2.0) must
			// NOT serialize as noise; on reimport NewTask re-derives the same
			// defaults, so the task round-trips identically.
			name: "no SLA", expected: nil, warn: DefaultSLAWarnMultiplier, fail: DefaultSLAFailMultiplier,
			wantExpected: nil, wantWarn: DefaultSLAWarnMultiplier, wantFail: DefaultSLAFailMultiplier,
			wantOmitted: true,
		},
	}

	for _, f := range formats {
		for _, c := range cases {
			t.Run(f.name+"/"+c.name, func(t *testing.T) {
				src := &Task{
					Prompt:                  "do the thing",
					ExpectedDurationMinutes: c.expected,
					SLAWarnMultiplier:       c.warn,
					SLAFailMultiplier:       c.fail,
					// Runtime SLA state that must NOT survive the round-trip.
					SLABreached:           true,
					ActualDurationSeconds: slaIntPtr(999),
				}
				rec, got := roundTripDefinition(t, src, f.marshal, f.unmarshal)

				// The export record carries SLA config iff an expectation is set.
				if c.wantOmitted {
					if rec.ExpectedDurationMinutes != nil || rec.SLAWarnMultiplier != 0 || rec.SLAFailMultiplier != 0 {
						t.Errorf("non-SLA task exported SLA config: expected=%v warn=%v fail=%v",
							rec.ExpectedDurationMinutes, rec.SLAWarnMultiplier, rec.SLAFailMultiplier)
					}
				} else if rec.ExpectedDurationMinutes == nil {
					t.Error("SLA task did not export expected_duration_minutes")
				}

				// The recreated task preserves the SLA expectation + thresholds.
				if !eqIntPtr(got.ExpectedDurationMinutes, c.wantExpected) {
					t.Errorf("expected_duration_minutes = %v, want %v", got.ExpectedDurationMinutes, c.wantExpected)
				}
				if got.SLAWarnMultiplier != c.wantWarn {
					t.Errorf("sla_warn_multiplier = %v, want %v", got.SLAWarnMultiplier, c.wantWarn)
				}
				if got.SLAFailMultiplier != c.wantFail {
					t.Errorf("sla_fail_multiplier = %v, want %v", got.SLAFailMultiplier, c.wantFail)
				}

				// Runtime-only SLA state must never cross the definition boundary.
				if got.SLABreached {
					t.Error("sla_breached leaked through export/import (runtime-only)")
				}
				if got.ActualDurationSeconds != nil {
					t.Errorf("actual_duration_seconds leaked through export/import: %v", got.ActualDurationSeconds)
				}
			})
		}
	}
}
