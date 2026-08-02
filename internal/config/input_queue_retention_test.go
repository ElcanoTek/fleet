package config

import (
	"strings"
	"testing"
)

func TestLoad_InputQueueRetentionDays(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.InputQueueRetentionDays != 30 {
		t.Fatalf("default retention = %d days, want 30", cfg.InputQueueRetentionDays)
	}

	t.Setenv("FLEET_INPUT_QUEUE_RETENTION_DAYS", "0")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load disabled retention: %v", err)
	}
	if cfg.InputQueueRetentionDays != 0 {
		t.Fatalf("disabled retention = %d days, want 0", cfg.InputQueueRetentionDays)
	}
}

func TestValidate_InputQueueRetentionDaysRejectsNegative(t *testing.T) {
	cfg := &Config{
		OpenRouterAPIKey:        "placeholder",
		SharedToken:             "placeholder",
		ConversationTTL:         1,
		UnpinnedCap:             1,
		InputQueueRetentionDays: -1,
		UploadMaxBytes:          1,
		DatabaseURL:             "postgres://placeholder@localhost/placeholder",
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "FLEET_INPUT_QUEUE_RETENTION_DAYS") {
		t.Fatalf("Validate error = %v, want input-queue retention error", err)
	}
}
