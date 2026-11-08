package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// slog hands ReplaceAttr only a group's members, so a secret-named GROUP
// ("credentials") used to let its innocently named members through. A member
// under any secret-named group on its path is redacted whole, whether the
// group is an attribute (nested too) or the logger's WithGroup; a number
// under it is kept, as under a secret-named key (#2074 review).
func TestSecretNamedGroupRedactsItsMembers(t *testing.T) {
	const opaque = "grpOpaqueValueNoShape1"
	cases := map[string]func(l *slog.Logger){
		"group attr": func(l *slog.Logger) {
			l.Info("x", slog.Group("credentials", "value", opaque, "token_count", 3))
		},
		"nested group": func(l *slog.Logger) {
			l.Info("x", slog.Group("connector", slog.Group("api_token", slog.Group("inner", "v", opaque), "token_count", 3)))
		},
		"WithGroup": func(l *slog.Logger) {
			l.WithGroup("Auth-Secret").Info("x", "value", opaque, "token_count", 3)
		},
		"WithGroup then With": func(l *slog.Logger) {
			l.WithGroup("credential").With("value", opaque).Info("x", "token_count", 3)
		},
	}
	for _, format := range []string{"json", "text"} {
		for name, logIt := range cases {
			t.Run(format+"/"+name, func(t *testing.T) {
				var buf bytes.Buffer
				logIt(slog.New(NewHandler(format, &buf, slog.LevelDebug)))
				out := buf.String()
				if strings.Contains(out, opaque) {
					t.Fatalf("member of a secret-named group leaked: %s", out)
				}
				if !strings.Contains(out, redactedValue) || !strings.Contains(out, "3") {
					t.Fatalf("want the member redacted and the count kept: %s", out)
				}
			})
		}
	}
	// An innocent group is untouched.
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x", slog.Group("connector", "name", "acme"))
	if !strings.Contains(buf.String(), `"name":"acme"`) {
		t.Fatalf("innocent group altered: %s", buf.String())
	}
}
