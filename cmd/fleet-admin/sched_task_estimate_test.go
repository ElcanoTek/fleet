// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. Used to assert the estimate subcommand's printed output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return buf.String()
}

func TestSchedTaskEstimateUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{name: "missing model", argv: []string{"--prompt", "hello"}},
		{name: "missing prompt", argv: []string{"--model", "anthropic/claude-sonnet-4-5"}},
		{name: "negative mcp-tools", argv: []string{"--model", "anthropic/claude-sonnet-4-5", "--prompt", "x", "--mcp-tools", "-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := schedTaskEstimate(tc.argv); code == 0 {
				t.Fatalf("expected non-zero exit for %s, got 0", tc.name)
			}
		})
	}
}

func TestSchedTaskEstimateKnownModelExitsZero(t *testing.T) {
	var code int
	out := captureStdout(t, func() {
		code = schedTaskEstimate([]string{
			"--model", "anthropic/claude-sonnet-4-5",
			"--prompt", "Summarize all issues opened in the last 7 days",
			"--max-iter", "20",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Estimated total:") {
		t.Errorf("expected a cost line for a known model, got:\n%s", out)
	}
	if !strings.Contains(out, "anthropic/claude-sonnet-4-5") {
		t.Errorf("expected the model name in output, got:\n%s", out)
	}
}

func TestSchedTaskEstimateUnknownModelOmitsCost(t *testing.T) {
	var code int
	out := captureStdout(t, func() {
		code = schedTaskEstimate([]string{
			"--model", "vendor/unknown-model",
			"--prompt", "do the thing",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 even for unknown pricing", code)
	}
	if strings.Contains(out, "Estimated total:") {
		t.Errorf("unknown model must not print a fabricated cost, got:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("expected the unknown-pricing note, got:\n%s", out)
	}
}

func TestSchedTaskEstimateJSON(t *testing.T) {
	var code int
	out := captureStdout(t, func() {
		code = schedTaskEstimate([]string{
			"--model", "anthropic/claude-sonnet-4-5",
			"--prompt", "hello",
			"--json",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var fc agentcore.CostForecast
	if err := json.Unmarshal([]byte(out), &fc); err != nil {
		t.Fatalf("output is not valid CostForecast JSON: %v\n%s", err, out)
	}
	if !fc.PricingKnown || fc.EstimatedTotalCostUSD == nil {
		t.Errorf("expected populated forecast for a known model, got %+v", fc)
	}
}
