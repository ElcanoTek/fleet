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
	if cfg.PersonaDefault != "victoria" {
		t.Errorf("PersonaDefault default: got %q", cfg.PersonaDefault)
	}
	if cfg.TitleModel != DefaultTitleModel {
		t.Errorf("TitleModel default: got %q, want %q", cfg.TitleModel, DefaultTitleModel)
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

func TestEmailMCPEnv_OmitsEmpty(t *testing.T) {
	cfg := &Config{
		AWSAccessKeyID: "A",
		AWSRegion:      "us-east-2",
	}
	env := cfg.EmailMCPEnv()
	if _, present := env["EMAIL_S3_BUCKET"]; present {
		t.Error("empty value should be omitted")
	}
	if env["AWS_REGION"] != "us-east-2" {
		t.Errorf("AWS_REGION: got %q", env["AWS_REGION"])
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
	if server, ok := cfg.MCPServers["sendgrid"]; !ok || !server.Enabled {
		t.Error("Expected SendGrid MCP server to be enabled")
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

func TestLoadWithAllMCPServers(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-full.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `
OPENROUTER_API_KEY=test-openrouter-key
SENDGRID_API_KEY=test-sendgrid-key
SENDGRID_FROM_EMAIL=test@example.com
AWS_ACCESS_KEY_ID=test-aws-key
AWS_SECRET_ACCESS_KEY=test-aws-secret
AWS_REGION=us-west-2
EMAIL_S3_BUCKET=test-bucket
EMAIL_S3_PREFIX=test-prefix/
`
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
	if cfg.SendGridAPIKey != "test-sendgrid-key" {
		t.Errorf("Expected SendGridAPIKey='test-sendgrid-key', got '%s'", cfg.SendGridAPIKey)
	}
	if cfg.SendGridFromEmail != "test@example.com" {
		t.Errorf("Expected SendGridFromEmail='test@example.com', got '%s'", cfg.SendGridFromEmail)
	}
	if cfg.AWSAccessKeyID != "test-aws-key" {
		t.Errorf("Expected AWSAccessKeyID='test-aws-key', got '%s'", cfg.AWSAccessKeyID)
	}
	if cfg.AWSRegion != "us-west-2" {
		t.Errorf("Expected AWSRegion='us-west-2', got '%s'", cfg.AWSRegion)
	}
	if cfg.EmailS3Bucket != "test-bucket" {
		t.Errorf("Expected EmailS3Bucket='test-bucket', got '%s'", cfg.EmailS3Bucket)
	}

	expectedServers := []string{"sendgrid", "email"}
	for _, server := range expectedServers {
		if _, ok := cfg.MCPServers[server]; !ok {
			t.Errorf("Expected MCP server '%s' to be configured", server)
		}
		if !cfg.MCPServers[server].Enabled {
			t.Errorf("Expected MCP server '%s' to be enabled", server)
		}
	}
}

func TestEmailMCPEnabledWithBucketOnly(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-email-bucket-only.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `EMAIL_S3_BUCKET=test-bucket`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	server, ok := cfg.MCPServers["email"]
	if !ok {
		t.Fatal("Expected email MCP server to be configured")
	}
	if !server.Enabled {
		t.Fatal("Expected email MCP server to be enabled")
	}
}

func TestEmailMCPDoesNotForwardEmptyOptionalEnvVars(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-email-optional-env.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `
EMAIL_S3_BUCKET=test-bucket
EMAIL_S3_DATE_PREFIX_FORMAT=
EMAIL_S3_MAX_DATE_PREFIX_DAYS=
`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	server, ok := cfg.MCPServers["email"]
	if !ok {
		t.Fatal("Expected email MCP server to be configured")
	}
	if _, ok := server.Env["EMAIL_S3_DATE_PREFIX_FORMAT"]; ok {
		t.Error("Expected EMAIL_S3_DATE_PREFIX_FORMAT to be omitted when empty")
	}
	if _, ok := server.Env["EMAIL_S3_MAX_DATE_PREFIX_DAYS"]; ok {
		t.Error("Expected EMAIL_S3_MAX_DATE_PREFIX_DAYS to be omitted when empty")
	}
}

func TestLoadEnvFileWithQuotes(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

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

func TestIndexExchangeMarketplaceAccountIDDefault(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	os.Unsetenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID")
	t.Setenv("INDEXEXCHANGE_USERNAME", "testuser")
	t.Setenv("INDEXEXCHANGE_PASSWORD", "testpass")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.IndexExchangeMarketplaceAccountID != "1491166" {
		t.Errorf("IndexExchangeMarketplaceAccountID default: expected %q, got %q",
			"1491166", cfg.IndexExchangeMarketplaceAccountID)
	}
}

func TestIndexExchangeMarketplaceAccountIDOverride(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	t.Setenv("INDEXEXCHANGE_USERNAME", "testuser")
	t.Setenv("INDEXEXCHANGE_PASSWORD", "testpass")
	t.Setenv("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", "1485234")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.IndexExchangeMarketplaceAccountID != "1485234" {
		t.Errorf("IndexExchangeMarketplaceAccountID override: expected %q, got %q",
			"1485234", cfg.IndexExchangeMarketplaceAccountID)
	}
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
	cases := map[string]bool{
		"PUBMATIC_OWNER_ID":           true,
		"PUBMATIC_OWNER_ID_REKLAIM":   true,
		"PUBMATIC_OWNER_ID_INFOLINKS": true,
		"OPENX_API_KEY":               true,
		"OPENX_API_KEY_REKLAIM":       true,
		"INDEXEXCHANGE_BASE_URL":      true,
		"PATH":                        false,
		"PATH_REKLAIM":                false,
		"LD_PRELOAD":                  false,
		"OPENX_API_KEY_reklaim":       false,
		"OPENX_API_KEY_":              false,
		"":                            false,
		"_REKLAIM":                    false,
	}
	for input, want := range cases {
		got := isAllowedEnvVar(input)
		if got != want {
			t.Errorf("isAllowedEnvVar(%q) = %v, want %v", input, got, want)
		}
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

	os.Setenv("OPENX_API_KEY", "")

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

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.OpenXAPIKey != "" {
		t.Errorf("Expected OpenXAPIKey='', got '%s'", cfg.OpenXAPIKey)
	}
	if _, ok := cfg.MCPServers["openx_mcp"]; ok {
		t.Error("Expected OpenX MCP server to be disabled")
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

func TestMCPServerNotEnabledWithoutCredentials(t *testing.T) {
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

	for name := range cfg.MCPServers {
		if name != "deal_sheet" {
			t.Errorf("Expected no credentialed MCP servers to be enabled without credentials, got %q", name)
		}
	}
}

func TestDefaultAWSRegion(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-aws.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `AWS_ACCESS_KEY_ID=test-key`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.AWSRegion != "us-east-2" {
		t.Errorf("Expected default AWSRegion='us-east-2', got '%s'", cfg.AWSRegion)
	}
}

func TestDefaultEmailS3Prefix(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-s3.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `EMAIL_S3_BUCKET=test-bucket`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.EmailS3Prefix != "emails/" {
		t.Errorf("Expected default EmailS3Prefix='emails/', got '%s'", cfg.EmailS3Prefix)
	}
}

func TestFastIOMCPServerConfiguration(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-fastio.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `FAST_IO_MCP_TOKEN=test-fastio-token`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.FastIOMCPToken != "test-fastio-token" {
		t.Errorf("Expected FastIOMCPToken='test-fastio-token', got '%s'", cfg.FastIOMCPToken)
	}

	fastIO, ok := cfg.MCPServers["fast_io"]
	if !ok {
		t.Fatal("Expected fast_io MCP server to be configured")
	}
	if !fastIO.Enabled {
		t.Error("Expected fast_io MCP server to be enabled")
	}
	if fastIO.Type != "http" {
		t.Errorf("Expected fast_io type='http', got '%s'", fastIO.Type)
	}
	if fastIO.URL != "https://mcp.fast.io/mcp" {
		t.Errorf("Expected fast_io URL='https://mcp.fast.io/mcp', got '%s'", fastIO.URL)
	}
	if fastIO.Headers == nil {
		t.Fatal("Expected fast_io Headers to be set")
	}
	if fastIO.Headers["Authorization"] != "Bearer test-fastio-token" {
		t.Errorf("Expected Authorization='Bearer test-fastio-token', got '%s'", fastIO.Headers["Authorization"])
	}
}

func TestFastIOMCPServerNotEnabledWithoutToken(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()

	tmpfile, err := os.CreateTemp("", "test-fastio-empty.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	content := `# No fast.io token`
	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if _, ok := cfg.MCPServers["fast_io"]; ok {
		t.Error("Expected fast_io MCP server to NOT be configured without token")
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
