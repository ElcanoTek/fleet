// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"net/http"

	"github.com/ElcanoTek/fleet/internal/boxdoctor"
)

// handleDoctor is the admin-only, READ-ONLY box-health report behind
// Settings → Admin → Doctor (GET /admin/doctor). It runs the in-process
// boxdoctor checks — DBs, disk headroom, rootless-podman prerequisites,
// sandbox image, sibling systemd units — from the service process's own
// vantage point, and each warn/fail carries the on-box repair command
// (almost always `sudo fleet doctor`, the script that actually FIXES drift;
// this endpoint never mutates the host).
//
// ?deep=1 additionally launches a throwaway sandbox container — the
// definitive smoke, but seconds-slow — so the UI requests it via an explicit
// button rather than on page load. Runs are serialized (doctorMu): concurrent
// admins queue instead of stampeding podman.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := boxdoctor.Options{Deep: truthyParam(r.URL.Query().Get("deep"))}
	if s.store != nil {
		opts.ChatPing = s.store.Ping
	}
	if s.cfg != nil {
		opts.SandboxImage = s.cfg.SandboxImage
	}
	if opts.SandboxImage == "" && s.clientConfig != nil {
		opts.SandboxImage = s.clientConfig.Sandbox().ResolvedImageRef()
	}
	s.doctorMu.Lock()
	defer s.doctorMu.Unlock()
	writeJSON(w, boxdoctor.Run(r.Context(), opts))
}

func truthyParam(v string) bool {
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
