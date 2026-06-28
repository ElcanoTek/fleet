package config

import (
	"testing"
	"time"
)

func TestLoad_DBPoolDefaults(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Defaults must reproduce the historical hard-coded behavior (#276).
	for _, tc := range []struct {
		name string
		got  DBPoolConfig
		ping time.Duration
	}{
		{"chat", cfg.ChatDBPool, 5 * time.Second},
		{"sched", cfg.SchedDBPool, 10 * time.Second},
	} {
		if tc.got.MaxOpenConns != 25 || tc.got.MaxIdleConns != 5 {
			t.Errorf("%s pool conns default: got %d/%d, want 25/5", tc.name, tc.got.MaxOpenConns, tc.got.MaxIdleConns)
		}
		// Historical behavior never called SetConnMaxIdleTime → 0 (unlimited);
		// lifetime was 5m. Defaults must reproduce that exactly.
		if tc.got.ConnMaxIdleTime != 0 || tc.got.ConnMaxLifetime != 5*time.Minute {
			t.Errorf("%s pool durations default: got idle=%s life=%s, want idle=0 (unlimited) life=5m", tc.name, tc.got.ConnMaxIdleTime, tc.got.ConnMaxLifetime)
		}
		if tc.got.ConnectTimeout != tc.ping {
			t.Errorf("%s connect timeout default: got %s, want %s", tc.name, tc.got.ConnectTimeout, tc.ping)
		}
	}
}

func TestLoad_DBPoolOverrides(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_CHAT_DB_MAX_CONNS", "40")
	t.Setenv("FLEET_CHAT_DB_MIN_CONNS", "8")
	t.Setenv("FLEET_CHAT_DB_MAX_CONN_IDLE_TIME", "90s")
	t.Setenv("FLEET_CHAT_DB_MAX_CONN_LIFETIME", "30m")
	t.Setenv("FLEET_CHAT_DB_CONNECT_TIMEOUT", "2s")
	t.Setenv("FLEET_SCHED_DB_MAX_CONNS", "12")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChatDBPool.MaxOpenConns != 40 || cfg.ChatDBPool.MaxIdleConns != 8 {
		t.Errorf("chat conns override: got %d/%d", cfg.ChatDBPool.MaxOpenConns, cfg.ChatDBPool.MaxIdleConns)
	}
	if cfg.ChatDBPool.ConnMaxIdleTime != 90*time.Second || cfg.ChatDBPool.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("chat duration override: idle=%s life=%s", cfg.ChatDBPool.ConnMaxIdleTime, cfg.ChatDBPool.ConnMaxLifetime)
	}
	if cfg.ChatDBPool.ConnectTimeout != 2*time.Second {
		t.Errorf("chat connect timeout override: got %s", cfg.ChatDBPool.ConnectTimeout)
	}
	// Sched picks up its own override but keeps defaults for the rest.
	if cfg.SchedDBPool.MaxOpenConns != 12 {
		t.Errorf("sched max conns override: got %d", cfg.SchedDBPool.MaxOpenConns)
	}
	if cfg.SchedDBPool.MaxIdleConns != 5 || cfg.SchedDBPool.ConnectTimeout != 10*time.Second {
		t.Errorf("sched untouched defaults drifted: idle=%d ping=%s", cfg.SchedDBPool.MaxIdleConns, cfg.SchedDBPool.ConnectTimeout)
	}
}

func TestGetenvFleetDuration_FallbackOnBadValue(t *testing.T) {
	isolateEnv(t)
	// Target a knob with a NON-ZERO default (lifetime = 5m) so the fallback is
	// distinguishable from a parsed zero.
	t.Setenv("FLEET_CHAT_DB_MAX_CONN_LIFETIME", "not-a-duration")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Unparseable duration falls back to the default rather than erroring.
	if cfg.ChatDBPool.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("bad duration should fall back to default 5m, got %s", cfg.ChatDBPool.ConnMaxLifetime)
	}
}
