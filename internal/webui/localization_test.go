// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"io"
	"net/http"
	"testing"
)

func TestLocalizationBootstrapKeepsPrivateCopyBehindMembership(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "member", false: "refused"}[allowed], func(t *testing.T) {
			e := newTestEnv(t)
			configCalls := 0
			e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/brand/meta":
					_, _ = io.WriteString(w, `{"login_title":"Welcome","has_localizations":true,"localizations":{"es":{"Welcome":"Bienvenido"}}}`)
				case "/client-config":
					configCalls++
					if !allowed {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_, _ = io.WriteString(w, `{"localizations":{"es":{"Private workflow":"Flujo privado"}}}`)
				default:
					_, _ = io.WriteString(w, `{}`)
				}
			}
			public := bootstrapOf(t, readBody(t, e.do(req("GET", "/login", ""))))
			if configCalls != 0 || len(public.Localizations) != 0 {
				t.Fatal("anonymous page requested private copy")
			}
			if public.Branding.Localizations["es"]["Welcome"] != "Bienvenido" {
				t.Fatal("public branding translation missing")
			}
			private := bootstrapOf(t, readBody(t, e.do(signedIn(t, req("GET", "/settings", "")))))
			if configCalls != 1 {
				t.Fatalf("config calls = %d", configCalls)
			}
			if allowed && private.Localizations["es"]["Private workflow"] != "Flujo privado" {
				t.Fatal("member translation missing")
			}
			if !allowed && len(private.Localizations) != 0 {
				t.Fatal("refused private copy exposed")
			}
			public = bootstrapOf(t, readBody(t, e.do(req("GET", "/login", ""))))
			if len(public.Localizations) != 0 || configCalls != 1 {
				t.Fatal("private translation leaked through cache")
			}
		})
	}
}
