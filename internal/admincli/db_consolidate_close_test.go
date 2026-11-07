// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	closeChat  = `postgres: ALTER DATABASE "chat" ALLOW_CONNECTIONS false`
	closeSched = `postgres: ALTER DATABASE "sched" ALLOW_CONNECTIONS false`
	openChat   = `postgres: ALTER DATABASE "chat" ALLOW_CONNECTIONS true`
	openSched  = `postgres: ALTER DATABASE "sched" ALLOW_CONNECTIONS true`
)

func indexOf(execs []string, want string) int {
	for i, e := range execs {
		if e == want {
			return i
		}
	}
	return -1
}

// cancellingPG interrupts the run (as SIGINT would) during the first copy.
type cancellingPG struct {
	*fakePG
	cancel context.CancelFunc
}

func (c *cancellingPG) DumpRestore(context.Context, string, string, string, string) error {
	c.cancel()
	return context.Canceled
}

// TestConsolidateClosesTheSources (#2055 review): writes after a source's
// copy would be lost and counts cannot tell, so both sources are closed to
// connections before anything is copied; they are re-opened on failure and
// on interrupt, and left closed on success.
func TestConsolidateClosesTheSources(t *testing.T) {
	clearDSNEnv(t)

	t.Run("success leaves them closed", func(t *testing.T) {
		f := healthyFake()
		var out bytes.Buffer
		if code := runConsolidate(t.Context(), consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
		ci, si, copyAt := indexOf(f.execs, closeChat), indexOf(f.execs, closeSched), indexOf(f.execs, `postgres: CREATE DATABASE "fleet_consolidate_chat" TEMPLATE "chat"`)
		if ci < 0 || si < 0 || copyAt < 0 || ci > copyAt || si > copyAt {
			t.Fatalf("the sources were not closed before the copy: %q", f.execs)
		}
		if indexOf(f.execs, openChat) >= 0 || indexOf(f.execs, openSched) >= 0 {
			t.Errorf("a successful move re-opened the sources: %q", f.execs)
		}
		if !strings.Contains(out.String(), "left CLOSED to connections") || !strings.Contains(out.String(), `ALLOW_CONNECTIONS true`) {
			t.Errorf("the output does not say the sources are closed or how to re-open them:\n%s", out.String())
		}
	})

	t.Run("failure re-opens them", func(t *testing.T) {
		f := healthyFake()
		f.dumpErr = errors.New("pg_restore: boom")
		var out bytes.Buffer
		if code := runConsolidate(t.Context(), consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 5 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
		if indexOf(f.execs, openChat) < indexOf(f.execs, closeChat) || indexOf(f.execs, openSched) < 0 {
			t.Errorf("a failed move left the sources closed: %q", f.execs)
		}
	})

	t.Run("interrupt re-opens them", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := healthyFake()
		pg := &cancellingPG{fakePG: f, cancel: cancel}
		var out bytes.Buffer
		d := testDeps(f, &out)
		d.pg = pg
		if code := runConsolidate(ctx, consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet", apply: true}, d); code != 5 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
		if indexOf(f.execs, openChat) < 0 || indexOf(f.execs, openSched) < 0 {
			t.Errorf("an interrupted move left the sources closed: %q", f.execs)
		}
	})

	t.Run("a lingering superuser session refuses and re-opens", func(t *testing.T) {
		f := healthyFake()
		f.prepend(fakeRule{frag: "GROUP BY 1, 2 ORDER BY 1", rows: [][]string{{"chat", "postgres", "1"}}})
		var out bytes.Buffer
		if code := runConsolidate(t.Context(), consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 5 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "chat: 1 session(s) as postgres") || indexOf(f.execs, openChat) < 0 {
			t.Errorf("want the refusal and the re-open:\n%s\n%q", out.String(), f.execs)
		}
		if indexOf(f.execs, `postgres: CREATE DATABASE "fleet_consolidate_chat" TEMPLATE "chat"`) >= 0 {
			t.Error("copied although a session was still open")
		}
	})

	t.Run("a source left closed is refused with the command that re-opens it", func(t *testing.T) {
		f := healthyFake()
		f.prepend(fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "f"}, {"sched", "sched", "t"}}})
		var out bytes.Buffer
		if code := runConsolidate(t.Context(), consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet"}, testDeps(f, &out)); code != 1 {
			t.Fatalf("exit %d:\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), `does not accept connections`) || !strings.Contains(out.String(), `ALTER DATABASE "chat" ALLOW_CONNECTIONS true`) {
			t.Errorf("output:\n%s", out.String())
		}
	})
}
