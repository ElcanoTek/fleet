package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ── shared test helpers ──

// isolateEnv clears every allowed-env key for the duration of the test and
// restores the original values on teardown (chat's helper, now over the union
// allowlist).
func isolateEnv(t *testing.T) {
	t.Helper()
	saved := map[string]*string{}
	for k := range allowedEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			vc := v
			saved[k] = &vc
		} else {
			saved[k] = nil
		}
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	})
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// clearEnvVars clears the cutlass-suite env keys (its scheduled tests rely on a
// known-clean slice rather than the full allowlist).
func clearEnvVars() {
	envVars := []string{
		"OPENROUTER_API_KEY",
		"LLM_PROVIDER_URL",
		"LLM_MODEL",
		"LLM_FALLBACK_MODELS",
		"OPENX_API_KEY",
		"SENDGRID_API_KEY",
		"SENDGRID_FROM_EMAIL",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_REGION",
		"EMAIL_S3_BUCKET",
		"EMAIL_S3_PREFIX",
		"EMAIL_S3_DATE_PREFIX_FORMAT",
		"EMAIL_S3_MAX_DATE_PREFIX_DAYS",
		"DOUBLE_QUOTED",
		"SINGLE_QUOTED",
		"NO_QUOTES",
		"SPACED_KEY",
		"TEST_VAR",
		"CUTLASS_TASK_MODEL",
		"CUTLASS_TASK_FALLBACK_MODEL",
		"CUTLASS_TASK_MAX_ITERATIONS",
		"CUTLASS_ALLOWED_DIRS",
		"GH_TOKEN",
		"FAST_IO_MCP_TOKEN",
		"INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID",
		"INDEXEXCHANGE_USERNAME",
		"INDEXEXCHANGE_PASSWORD",
	}
	for _, v := range envVars {
		os.Unsetenv(v)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Interactive (chat) config suite — ported verbatim.
// ──────────────────────────────────────────────────────────────────────────

func TestLoad_DefaultsApply(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr default: got %q", cfg.Addr)
	}
	if cfg.ConversationTTL != 14 {
		t.Errorf("ConversationTTL default: got %d", cfg.ConversationTTL)
	}
	if cfg.UnpinnedCap != 50 {
		t.Errorf("UnpinnedCap default: got %d", cfg.UnpinnedCap)
	}
	if !cfg.ReasoningEnabled {
		t.Error("ReasoningEnabled default: expected true")
	}
	if cfg.PersonaDefault != "assistant" {
		t.Errorf("PersonaDefault default: got %q", cfg.PersonaDefault)
	}
	if cfg.TitleModel != DefaultTitleModel {
		t.Errorf("TitleModel default: got %q, want %q", cfg.TitleModel, DefaultTitleModel)
	}
	if cfg.ShutdownGraceSeconds != 30 {
		t.Errorf("ShutdownGraceSeconds default: got %d, want 30", cfg.ShutdownGraceSeconds)
	}
}

func TestLoad_ShutdownGraceOverride(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_SHUTDOWN_GRACE_SECONDS", "5")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShutdownGraceSeconds != 5 {
		t.Errorf("ShutdownGraceSeconds: got %d, want 5", cfg.ShutdownGraceSeconds)
	}
}

func TestLoad_LogSinkDefaultsOff(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Default OFF: no file set means the rotating file sink stays disabled and the
	// process keeps logging to stderr exactly as before (journald rotates that).
	if cfg.Log.File != "" {
		t.Errorf("Log.File default: got %q, want empty (sink off)", cfg.Log.File)
	}
	// The size/age/backup/compress knobs still carry their defaults so an operator
	// who later sets only FLEET_LOG_FILE gets sensible rotation.
	if cfg.Log.MaxSizeMB != 100 {
		t.Errorf("Log.MaxSizeMB default: got %d, want 100", cfg.Log.MaxSizeMB)
	}
	if cfg.Log.MaxAgeDays != 0 {
		t.Errorf("Log.MaxAgeDays default: got %d, want 0", cfg.Log.MaxAgeDays)
	}
	if cfg.Log.MaxBackups != 7 {
		t.Errorf("Log.MaxBackups default: got %d, want 7", cfg.Log.MaxBackups)
	}
	if !cfg.Log.Compress {
		t.Error("Log.Compress default: want true")
	}
}

func TestLoad_LogSinkOverrides(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_LOG_FILE", "/var/log/fleet/fleet.log")
	t.Setenv("FLEET_LOG_MAX_SIZE_MB", "50")
	t.Setenv("FLEET_LOG_MAX_AGE_DAYS", "30")
	t.Setenv("FLEET_LOG_MAX_BACKUPS", "3")
	t.Setenv("FLEET_LOG_COMPRESS", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.File != "/var/log/fleet/fleet.log" {
		t.Errorf("Log.File: got %q", cfg.Log.File)
	}
	if cfg.Log.MaxSizeMB != 50 {
		t.Errorf("Log.MaxSizeMB: got %d, want 50", cfg.Log.MaxSizeMB)
	}
	if cfg.Log.MaxAgeDays != 30 {
		t.Errorf("Log.MaxAgeDays: got %d, want 30", cfg.Log.MaxAgeDays)
	}
	if cfg.Log.MaxBackups != 3 {
		t.Errorf("Log.MaxBackups: got %d, want 3", cfg.Log.MaxBackups)
	}
	if cfg.Log.Compress {
		t.Error("Log.Compress: want false after FLEET_LOG_COMPRESS=false")
	}
}

func TestLoad_SandboxDiskGB(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Default 0 → the sandbox layer applies its own default (5 GiB).
	if cfg.SandboxDiskGB != 0 {
		t.Errorf("SandboxDiskGB default: got %d, want 0 (sandbox-default sentinel)", cfg.SandboxDiskGB)
	}

	t.Setenv("FLEET_SANDBOX_DISK_GB", "20")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxDiskGB != 20 {
		t.Errorf("SandboxDiskGB override: got %d, want 20", cfg.SandboxDiskGB)
	}
}

func TestLoad_TitleModelOverride(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("CHAT_TITLE_MODEL", "~anthropic/claude-sonnet-latest")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TitleModel != "~anthropic/claude-sonnet-latest" {
		t.Errorf("TitleModel override: got %q", cfg.TitleModel)
	}
}

func TestLoad_LocalFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	localPath := filepath.Join(dir, ".env.local")
	content := `
# comment line
OPENROUTER_API_KEY="local-key"
PERSONA_DEFAULT="local-persona"
`
	if err := os.WriteFile(localPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write local: %v", err)
	}

	cfg, err := Load(localPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenRouterAPIKey != "local-key" {
		t.Errorf("OpenRouterAPIKey: got %q", cfg.OpenRouterAPIKey)
	}
	if cfg.PersonaDefault != "local-persona" {
		t.Errorf("PersonaDefault: got %q", cfg.PersonaDefault)
	}
}

func TestLoad_ProcessEnvBeatsFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	local := filepath.Join(dir, ".env.local")
	_ = os.WriteFile(local, []byte(`OPENROUTER_API_KEY="local"`+"\n"), 0o600)

	t.Setenv("OPENROUTER_API_KEY", "from-env")

	cfg, err := Load(local)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenRouterAPIKey != "from-env" {
		t.Errorf("got %q, want from-env", cfg.OpenRouterAPIKey)
	}
}

func TestLoad_MissingEnvFileOk(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	if _, err := Load("/nonexistent/env"); err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
}

func TestLoad_IgnoresUnknownKeys(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	localPath := filepath.Join(dir, ".env.local")
	_ = os.WriteFile(localPath, []byte(`LD_PRELOAD="evil.so"`+"\n"), 0o600)
	if _, err := Load(localPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("LD_PRELOAD"); got != "" {
		t.Errorf("LD_PRELOAD leaked into env: %q", got)
	}
}

func TestLoad_AllowsPrefixedKeys(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	// Open-ended per-user credential prefixes are admitted by the client bundle
	// at startup (no longer statically allowlisted). Register the prefix so its
	// suffixed variants flow from the .env file.
	RegisterAllowedEnvPrefixes("GAMMA_API_KEY_")

	_ = os.WriteFile(filepath.Join(dir, ".env.local"), []byte(
		`GAMMA_API_KEY_BRAD="sk-gamma-brad"`+"\n"+
			`GAMMA_API_KEY_ELYSE="sk-gamma-elyse"`+"\n",
	), 0o600)

	envFile := filepath.Join(dir, ".env.local")
	if _, err := Load(envFile); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("GAMMA_API_KEY_BRAD"); got != "sk-gamma-brad" {
		t.Errorf("GAMMA_API_KEY_BRAD = %q, want sk-gamma-brad", got)
	}
	if got := os.Getenv("GAMMA_API_KEY_ELYSE"); got != "sk-gamma-elyse" {
		t.Errorf("GAMMA_API_KEY_ELYSE = %q, want sk-gamma-elyse", got)
	}
	// cleanup the prefixed keys this test set
	t.Cleanup(func() {
		os.Unsetenv("GAMMA_API_KEY_BRAD")
		os.Unsetenv("GAMMA_API_KEY_ELYSE")
	})
}

func TestLoad_PrefixDoesNotMatchUnrelatedKeys(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	_ = os.WriteFile(filepath.Join(dir, ".env.local"), []byte(
		`GAMMA_API_KEY_=should-be-skipped-empty-suffix`+"\n"+
			`SOME_GAMMA_API_KEY=evil`+"\n"+
			`LD_PRELOAD_GAMMA_API_KEY=worse`+"\n",
	), 0o600)

	envFile := filepath.Join(dir, ".env.local")
	if _, err := Load(envFile); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("SOME_GAMMA_API_KEY"); got != "" {
		t.Errorf("SOME_GAMMA_API_KEY leaked: %q", got)
	}
	if got := os.Getenv("LD_PRELOAD_GAMMA_API_KEY"); got != "" {
		t.Errorf("LD_PRELOAD_GAMMA_API_KEY leaked: %q", got)
	}
}

func TestLoad_StripsQuotes(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	localPath := filepath.Join(dir, ".env.local")
	_ = os.WriteFile(localPath, []byte(`OPENROUTER_API_KEY="quoted-value"`+"\n"), 0o600)

	cfg, err := Load(localPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenRouterAPIKey != "quoted-value" {
		t.Errorf("quote strip: got %q", cfg.OpenRouterAPIKey)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"ok", func(c *Config) {
			c.OpenRouterAPIKey = "k"
			c.SharedToken = "t"
			c.ConversationTTL = 1
			c.UnpinnedCap = 1
			c.DatabaseURL = "postgres://x@localhost/x"
		}, false},
		{"missing openrouter", func(c *Config) {
			c.SharedToken = "t"
			c.ConversationTTL = 1
			c.UnpinnedCap = 1
			c.DatabaseURL = "postgres://x@localhost/x"
		}, true},
		{"missing shared token", func(c *Config) {
			c.OpenRouterAPIKey = "k"
			c.ConversationTTL = 1
			c.UnpinnedCap = 1
			c.DatabaseURL = "postgres://x@localhost/x"
		}, true},
		{"bad ttl", func(c *Config) {
			c.OpenRouterAPIKey = "k"
			c.SharedToken = "t"
			c.UnpinnedCap = 1
			c.DatabaseURL = "postgres://x@localhost/x"
		}, true},
		{"bad cap", func(c *Config) {
			c.OpenRouterAPIKey = "k"
			c.SharedToken = "t"
			c.ConversationTTL = 1
			c.DatabaseURL = "postgres://x@localhost/x"
		}, true},
		{"missing database url", func(c *Config) {
			c.OpenRouterAPIKey = "k"
			c.SharedToken = "t"
			c.ConversationTTL = 1
			c.UnpinnedCap = 1
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestLockdownAllows(t *testing.T) {
	cfg := &Config{
		LockdownAllowedModels: []string{
			"google/gemini-3-flash-preview",
			"anthropic/claude-sonnet-4.6",
		},
	}
	cases := []struct {
		slug string
		want bool
	}{
		{"google/gemini-3-flash-preview", true},
		{"anthropic/claude-sonnet-4.6", true},
		{"  anthropic/claude-sonnet-4.6  ", true},
		{"openai/gpt-5", false},
		{"", false},
		{"Anthropic/claude-sonnet-4.6", false},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			if got := cfg.LockdownAllows(tc.slug); got != tc.want {
				t.Errorf("LockdownAllows(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}

func TestSplitLockdownModels_DefaultsWhenEmpty(t *testing.T) {
	got := splitLockdownModels("")
	if len(got) < 2 {
		t.Fatalf("expected default list with both tier slots, got %v", got)
	}
	wantContains := []string{
		"google/gemini-3.5-flash",
		"anthropic/claude-opus-4.8",
	}
	for _, w := range wantContains {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default list missing %q (got %v)", w, got)
		}
	}
	for _, g := range got {
		if g == "~moonshotai/kimi-latest" {
			t.Errorf("default list still contains the removed economy tier %q (got %v)", g, got)
		}
	}
}

func TestSplitLockdownModels_ParsesCSV(t *testing.T) {
	got := splitLockdownModels(" foo/bar , baz/quux ,  ")
	want := []string{"foo/bar", "baz/quux"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, g, want[i])
		}
	}
}

func TestLockdownAvailable(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"image set", &Config{SandboxImage: "ghcr.io/x/y:1"}, true},
		{"image empty", &Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.LockdownAvailable(); got != tc.want {
				t.Errorf("LockdownAvailable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoad_LockdownOnlyRequiresImage(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CHAT_LOCKDOWN_ONLY", "true")
	t.Setenv("CHAT_SANDBOX_IMAGE", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LockdownOnly {
		t.Error("LockdownOnly should be silently disabled when SandboxImage is unset")
	}
	if cfg.LockdownAvailable() {
		t.Error("LockdownAvailable() should be false when SandboxImage is unset")
	}
}

func TestLoad_LockdownOnlyRespectedWithImage(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CHAT_LOCKDOWN_ONLY", "true")
	t.Setenv("CHAT_SANDBOX_IMAGE", "ghcr.io/example/sandbox:test")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LockdownOnly {
		t.Error("LockdownOnly should be true when both env vars are set")
	}
	if !cfg.LockdownAvailable() {
		t.Error("LockdownAvailable() should be true when SandboxImage is set")
	}
}

func TestLoad_LockdownAllowedModelsEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CHAT_LOCKDOWN_ALLOWED_MODELS", "anthropic/claude-opus-4.7,openai/gpt-5")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"anthropic/claude-opus-4.7", "openai/gpt-5"}
	if len(cfg.LockdownAllowedModels) != len(want) {
		t.Fatalf("got %v, want %v", cfg.LockdownAllowedModels, want)
	}
	for i, m := range cfg.LockdownAllowedModels {
		if m != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, m, want[i])
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Scheduled (cutlass) config suite — ported; colliding names disambiguated.
// ──────────────────────────────────────────────────────────────────────────

func TestLoadScheduled(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `
OPENROUTER_API_KEY=test-key
SENDGRID_API_KEY=sendgrid-key
`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.OpenRouterAPIKey != "test-key" {
		t.Errorf("Expected OpenRouterAPIKey=test-key, got %s", cfg.OpenRouterAPIKey)
	}
	// The MCP catalog is no longer built by config.Load; it is sourced from the
	// client bundle's manifest and assigned by cmd/fleet. After Load it is empty.
	if len(cfg.MCPServers) != 0 {
		t.Errorf("Expected empty MCPServers after Load, got %v", cfg.MCPServers)
	}
}

func TestLoadNonExistentFile(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	cfg, err := Load("/nonexistent/file.env")
	if err != nil {
		t.Fatalf("Should not error on non-existent file: %v", err)
	}
	if cfg == nil {
		t.Error("Expected non-nil config")
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	os.Setenv("TEST_VAR", "test-value")
	defer os.Unsetenv("TEST_VAR")

	if val := getEnvOrDefault("TEST_VAR", "default"); val != "test-value" {
		t.Errorf("Expected test-value, got %s", val)
	}
	if val := getEnvOrDefault("NONEXISTENT_VAR", "default"); val != "default" {
		t.Errorf("Expected default, got %s", val)
	}
}

// TestGetEnvOrDefaultIntFloat_RejectTrailingGarbage proves the strconv-based
// parsers reject trailing garbage (the #134 fix: "12abc"/"0.3xyz" must fall back
// to the default, not silently parse as 12/0.3 the way fmt.Sscanf did), while
// accepting clean values with surrounding whitespace.
func TestGetEnvOrDefaultIntFloat_RejectTrailingGarbage(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		t.Setenv("TEST_INT", "12abc")
		if got := getEnvOrDefaultInt("TEST_INT", 7); got != 7 {
			t.Errorf("trailing garbage: got %d, want default 7", got)
		}
		t.Setenv("TEST_INT", "  12  ")
		if got := getEnvOrDefaultInt("TEST_INT", 7); got != 12 {
			t.Errorf("trimmed value: got %d, want 12", got)
		}
		os.Unsetenv("TEST_INT")
		if got := getEnvOrDefaultInt("TEST_INT", 7); got != 7 {
			t.Errorf("unset: got %d, want default 7", got)
		}
	})
	t.Run("float", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "0.3xyz")
		if got := getEnvOrDefaultFloat("TEST_FLOAT", 1.5); got != 1.5 {
			t.Errorf("trailing garbage: got %v, want default 1.5", got)
		}
		t.Setenv("TEST_FLOAT", " 0.3 ")
		if got := getEnvOrDefaultFloat("TEST_FLOAT", 1.5); got != 0.3 {
			t.Errorf("trimmed value: got %v, want 0.3", got)
		}
	})
}

// TestLoad_AllowsFleetOpenRouterBaseURL proves the canonical-prefixed fake-LLM
// seam (FLEET_OPENROUTER_BASE_URL) survives the .env allowlist (#134) so a dev
// pointing at the fake LLM via .env.local is honored rather than silently
// dropped and sent to real OpenRouter.
func TestLoad_AllowsFleetOpenRouterBaseURL(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	localPath := filepath.Join(dir, ".env.local")
	_ = os.WriteFile(localPath, []byte(`FLEET_OPENROUTER_BASE_URL="http://127.0.0.1:9/v1"`+"\n"), 0o600)
	if _, err := Load(localPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("FLEET_OPENROUTER_BASE_URL"); got != "http://127.0.0.1:9/v1" {
		t.Errorf("FLEET_OPENROUTER_BASE_URL dropped by allowlist: %q", got)
	}
}

func TestLoadEnvFileWithQuotes(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	// OPENX_API_KEY is a connector credential the client bundle registers at
	// startup; register it here so it flows from the .env file under test.
	RegisterAllowedEnvVars("OPENX_API_KEY")

	tmpfile, err := os.CreateTemp("", "test-quotes.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `
# This is a comment
OPENX_API_KEY="double-quoted-value"
SENDGRID_API_KEY='single-quoted-value'
TAVILY_API_KEY=no-quotes-value

# Another comment
LOG_LEVEL = spaced-value
`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if val := os.Getenv("OPENX_API_KEY"); val != "double-quoted-value" {
		t.Errorf("Expected 'double-quoted-value', got '%s'", val)
	}
	if val := os.Getenv("SENDGRID_API_KEY"); val != "single-quoted-value" {
		t.Errorf("Expected 'single-quoted-value', got '%s'", val)
	}
	if val := os.Getenv("TAVILY_API_KEY"); val != "no-quotes-value" {
		t.Errorf("Expected 'no-quotes-value', got '%s'", val)
	}
	if val := os.Getenv("LOG_LEVEL"); val != "spaced-value" {
		t.Errorf("Expected 'spaced-value', got '%s'", val)
	}
	t.Cleanup(func() {
		os.Unsetenv("OPENX_API_KEY")
		os.Unsetenv("SENDGRID_API_KEY")
		os.Unsetenv("TAVILY_API_KEY")
		os.Unsetenv("LOG_LEVEL")
	})
}

func TestLoadEnvFile_AcceptsSuffixedVariantKeys(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	// DSP connector credentials are no longer in the static allowlist; the client
	// bundle admits them at startup. Register the base names so their per-account
	// "<BASE>_<SUFFIX>" variants are admitted by the suffix rule. PATH is NOT
	// registered, so PATH_REKLAIM must still be rejected.
	RegisterAllowedEnvVars("PUBMATIC_OWNER_ID", "OPENX_API_KEY", "INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID")

	tmpfile, err := os.CreateTemp("", "test-suffixed.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `
PUBMATIC_OWNER_ID=60067 # Elcano default
PUBMATIC_OWNER_ID_REKLAIM=50751
PUBMATIC_OWNER_ID_INFOLINKS=65421
PATH_REKLAIM=should-be-rejected
OPENX_API_KEY_REKLAIM=reklaim-openx-key
INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID=1491166
INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_REKLAIM=1485234
INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_ZETA=1507580
`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	if _, err := Load(tmpfile.Name()); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if val := os.Getenv("PUBMATIC_OWNER_ID"); val != "60067" {
		t.Errorf("PUBMATIC_OWNER_ID: expected %q (inline comment stripped), got %q", "60067", val)
	}
	if val := os.Getenv("PUBMATIC_OWNER_ID_REKLAIM"); val != "50751" {
		t.Errorf("PUBMATIC_OWNER_ID_REKLAIM: expected %q, got %q", "50751", val)
	}
	if val := os.Getenv("PUBMATIC_OWNER_ID_INFOLINKS"); val != "65421" {
		t.Errorf("PUBMATIC_OWNER_ID_INFOLINKS: expected %q, got %q", "65421", val)
	}
	if val := os.Getenv("OPENX_API_KEY_REKLAIM"); val != "reklaim-openx-key" {
		t.Errorf("OPENX_API_KEY_REKLAIM: expected %q, got %q", "reklaim-openx-key", val)
	}
	if val := os.Getenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID"); val != "1491166" {
		t.Errorf("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID: expected %q, got %q", "1491166", val)
	}
	if val := os.Getenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_REKLAIM"); val != "1485234" {
		t.Errorf("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_REKLAIM: expected %q, got %q", "1485234", val)
	}
	if val := os.Getenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_ZETA"); val != "1507580" {
		t.Errorf("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_ZETA: expected %q, got %q", "1507580", val)
	}
	if val := os.Getenv("PATH_REKLAIM"); val != "" {
		t.Errorf("PATH_REKLAIM should be rejected (PATH not in allowlist), got %q", val)
	}
	t.Cleanup(func() {
		for _, k := range []string{
			"PUBMATIC_OWNER_ID", "PUBMATIC_OWNER_ID_REKLAIM", "PUBMATIC_OWNER_ID_INFOLINKS",
			"OPENX_API_KEY_REKLAIM", "INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID",
			"INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_REKLAIM", "INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID_ZETA",
		} {
			os.Unsetenv(k)
		}
	})
}

func TestStripInlineComment(t *testing.T) {
	cases := map[string]string{
		"60067 # Elcano default":   "60067",
		"50751\t# tab-then-hash":   "50751",
		"plain-value":              "plain-value",
		"value-with-#-mid":         "value-with-#-mid",
		`"60067 # quoted"`:         `"60067 # quoted"`,
		`'50751 # also quoted'`:    `'50751 # also quoted'`,
		"":                         "",
		"60067    # padded spaces": "60067",
	}
	for input, want := range cases {
		got := stripInlineComment(input)
		if got != want {
			t.Errorf("stripInlineComment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsAllowedEnvVar(t *testing.T) {
	// DSP connector credentials are no longer in the static allowlist; a client
	// bundle registers them at startup. Register a couple here so the suffix-rule
	// cases below exercise the registered-base path.
	RegisterAllowedEnvVars("PUBMATIC_OWNER_ID", "OPENX_API_KEY")

	cases := map[string]bool{
		// generic static-allowlist entries
		"OPENROUTER_API_KEY": true,
		"TAVILY_API_KEY":     true,
		"FLEET_SERVER_ADDR":  true,
		// registered bases + their per-account suffix variants
		"PUBMATIC_OWNER_ID":           true,
		"PUBMATIC_OWNER_ID_REKLAIM":   true,
		"PUBMATIC_OWNER_ID_INFOLINKS": true,
		"OPENX_API_KEY":               true,
		"OPENX_API_KEY_REKLAIM":       true,
		// rejections
		"INDEXEXCHANGE_BASE_URL": false, // base not registered
		"PATH":                   false,
		"PATH_REKLAIM":           false,
		"LD_PRELOAD":             false,
		"OPENX_API_KEY_reklaim":  false, // suffix must be uppercase
		"OPENX_API_KEY_":         false, // empty suffix
		"":                       false,
		"_REKLAIM":               false,
	}
	for input, want := range cases {
		got := isAllowedEnvVar(input)
		if got != want {
			t.Errorf("isAllowedEnvVar(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestRegisterAllowedEnvVars(t *testing.T) {
	const fresh = "FLEET_TEST_REGISTERED_CONNECTOR_KEY"
	if isAllowedEnvVar(fresh) {
		t.Fatalf("%q should not be allowed before registration", fresh)
	}
	RegisterAllowedEnvVars(fresh)
	if !isAllowedEnvVar(fresh) {
		t.Errorf("%q should be allowed after RegisterAllowedEnvVars", fresh)
	}
	// A per-account suffix variant of a registered base is admitted too.
	if !isAllowedEnvVar(fresh + "_REKLAIM") {
		t.Errorf("%q_REKLAIM should be allowed via the suffix rule", fresh)
	}
}

func TestLoadDoesNotOverrideExistingEnv(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	os.Setenv("OPENROUTER_API_KEY", "existing-key")

	tmpfile, err := os.CreateTemp("", "test-override.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `OPENROUTER_API_KEY=new-key`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.OpenRouterAPIKey != "existing-key" {
		t.Errorf("Expected OpenRouterAPIKey='existing-key', got '%s'", cfg.OpenRouterAPIKey)
	}
}

func TestLoadOverridesWithEmptyString(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	// A registered connector credential set to an empty string in the process env
	// must beat the file value (process env wins, even when empty).
	RegisterAllowedEnvVars("OPENX_API_KEY")
	os.Setenv("OPENX_API_KEY", "")
	defer os.Unsetenv("OPENX_API_KEY")

	tmpfile, err := os.CreateTemp("", "test-empty-override.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `OPENX_API_KEY=default-openx-key`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	if _, err := Load(tmpfile.Name()); err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if got := os.Getenv("OPENX_API_KEY"); got != "" {
		t.Errorf("Expected OPENX_API_KEY='' (process env wins), got '%s'", got)
	}
}

func TestLoadIgnoresUserModelOverrides(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-llm-provider.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := "OPENROUTER_API_KEY=test-openrouter-key\nLLM_PROVIDER_URL=http://custom-llm:11434/v1\nLLM_MODEL=openrouter/auto\nLLM_FALLBACK_MODELS=openai/gpt-5.2"
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.OpenRouterAPIKey != "test-openrouter-key" {
		t.Errorf("Expected OpenRouterAPIKey='test-openrouter-key', got '%s'", cfg.OpenRouterAPIKey)
	}
	if got := os.Getenv("LLM_PROVIDER_URL"); got != "" {
		t.Errorf("Expected LLM_PROVIDER_URL override to be ignored, got '%s'", got)
	}
	if got := os.Getenv("LLM_MODEL"); got != "" {
		t.Errorf("Expected LLM_MODEL override to be ignored, got '%s'", got)
	}
	if got := os.Getenv("LLM_FALLBACK_MODELS"); got != "" {
		t.Errorf("Expected LLM_FALLBACK_MODELS override to be ignored, got '%s'", got)
	}
}

func TestLoadWithOpenRouterDefaults(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-openrouter.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `OPENROUTER_API_KEY=test-openrouter-key`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.OpenRouterAPIKey != "test-openrouter-key" {
		t.Errorf("Expected OpenRouterAPIKey='test-openrouter-key', got '%s'", cfg.OpenRouterAPIKey)
	}
}

func TestLoadPreservesTaskModelOverride(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	os.Setenv("CUTLASS_TASK_MODEL", "deepseek/deepseek-v3.2")
	defer os.Unsetenv("CUTLASS_TASK_MODEL")

	tmpfile, err := os.CreateTemp("", "test-task-model.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte("OPENROUTER_API_KEY=test-openrouter-key\n")); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.TaskModel != "deepseek/deepseek-v3.2" {
		t.Fatalf("Expected TaskModel=deepseek/deepseek-v3.2, got %q", cfg.TaskModel)
	}
}

func TestMCPServersEmptyAfterLoad(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-empty.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `# Empty config`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// config.Load no longer builds an MCP catalog; cmd/fleet assigns one sourced
	// from the client bundle's manifest. After Load the map is non-nil but empty.
	if cfg.MCPServers == nil {
		t.Fatal("Expected MCPServers to be a non-nil (empty) map after Load")
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("Expected no MCP servers after Load, got %v", cfg.MCPServers)
	}
}

func TestValidateScheduled(t *testing.T) {
	valid := &Config{
		OpenRouterAPIKey: "sk-or-v1-abc123",
		MaxIterations:    500,
		MaxCostUSD:       10.0,
		MaxTotalTokens:   1000000,
		LLMMaxTokens:     65536,
	}
	if err := valid.ValidateScheduled(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}

	bad := *valid
	bad.OpenRouterAPIKey = ""
	if err := bad.ValidateScheduled(); err == nil {
		t.Fatal("expected error for missing API key")
	}

	bad = *valid
	bad.OpenRouterAPIKey = "wrong-prefix-key"
	if err := bad.ValidateScheduled(); err == nil {
		t.Fatal("expected error for wrong API key prefix")
	}

	bad = *valid
	bad.MaxIterations = 0
	if err := bad.ValidateScheduled(); err == nil {
		t.Fatal("expected error for zero max iterations")
	}

	bad.MaxIterations = 99999
	if err := bad.ValidateScheduled(); err == nil {
		t.Fatal("expected error for excessive max iterations")
	}

	bad = *valid
	bad.LLMMaxTokens = 100
	if err := bad.ValidateScheduled(); err == nil {
		t.Fatal("expected error for too-low LLM max tokens")
	}
}

func TestLoadWithQuotedEnvFromPodman(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	os.Setenv("OPENROUTER_API_KEY", `"sk-or-v1-abc123def"`)

	cfg, err := Load("/nonexistent/.env")
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.OpenRouterAPIKey != "sk-or-v1-abc123def" {
		t.Errorf("Expected quotes stripped, got: %q", cfg.OpenRouterAPIKey)
	}
	if err := cfg.ValidateScheduled(); err != nil {
		t.Errorf("Validation should pass with stripped key, got: %v", err)
	}
}

func TestLoadWithSingleQuotedEnvFromPodman(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	os.Setenv("OPENROUTER_API_KEY", `'sk-or-v1-xyz789'`)

	cfg, err := Load("/nonexistent/.env")
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.OpenRouterAPIKey != "sk-or-v1-xyz789" {
		t.Errorf("Expected quotes stripped, got: %q", cfg.OpenRouterAPIKey)
	}
}

func TestLoad_IPAccessControlDefaultsEmpty(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.IPAllowlist) != 0 {
		t.Errorf("IPAllowlist default: got %v, want empty (allow-all)", cfg.IPAllowlist)
	}
	if len(cfg.IPDenylist) != 0 {
		t.Errorf("IPDenylist default: got %v, want empty", cfg.IPDenylist)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies default: got %v, want empty", cfg.TrustedProxies)
	}
}

func TestLoad_IPAccessControlParses(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_IP_ALLOWLIST", "192.168.1.0/24, 10.0.0.0/8, 203.0.113.7")
	t.Setenv("FLEET_IP_DENYLIST", "45.33.32.156")
	t.Setenv("FLEET_TRUSTED_PROXIES", "127.0.0.1, ::1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.IPAllowlist); got != 3 {
		t.Fatalf("IPAllowlist len = %d, want 3", got)
	}
	// Bare host coerced to /32.
	if s := cfg.IPAllowlist[2].String(); s != "203.0.113.7/32" {
		t.Errorf("bare host CIDR = %q, want 203.0.113.7/32", s)
	}
	if got := len(cfg.IPDenylist); got != 1 {
		t.Errorf("IPDenylist len = %d, want 1", got)
	}
	if got := len(cfg.TrustedProxies); got != 2 {
		t.Errorf("TrustedProxies len = %d, want 2", got)
	}
}

func TestLoad_IPAccessControlMalformedIsFatal(t *testing.T) {
	cases := map[string]string{
		"FLEET_IP_ALLOWLIST":    "not-a-cidr",
		"FLEET_IP_DENYLIST":     "10.0.0.0/99",
		"FLEET_TRUSTED_PROXIES": "999.999.999.999",
	}
	for envKey, badVal := range cases {
		t.Run(envKey, func(t *testing.T) {
			isolateEnv(t)
			chdir(t, t.TempDir())
			t.Setenv(envKey, badVal)
			if _, err := Load(""); err == nil {
				t.Fatalf("Load with malformed %s=%q: want error, got nil", envKey, badVal)
			}
		})
	}
}
