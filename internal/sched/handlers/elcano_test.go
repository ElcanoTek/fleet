// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestElcanoLogout(t *testing.T) {
	h := &Handlers{config: Config{ElcanoCookieName: "elcano_auth", ElcanoCookieDomain: "elcanotek.com"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rr := httptest.NewRecorder()
	h.ElcanoLogout(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / (the server's own login)", loc)
	}
	// Must emit an expiring deletion cookie matching name + domain so the
	// browser actually drops the shared elcano_auth cookie.
	sc := rr.Header().Get("Set-Cookie")
	if !strings.Contains(sc, "elcano_auth=;") || !strings.Contains(sc, "Domain=elcanotek.com") || !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("Set-Cookie = %q, want an expired elcano_auth on Domain=elcanotek.com", sc)
	}
}
