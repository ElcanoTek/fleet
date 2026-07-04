// One-click Rampart service install (internal/rampartinstall): the Features
// panel's "Install Rampart service" button. POST kicks the async build+run
// job, GET polls its key-free status, DELETE removes the managed container.
// Admin-gated like the rest of /admin/*.

package httpapi

import (
	"context"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/rampartinstall"
)

// piiRampartInstaller is the seam the endpoints call, satisfied by
// *rampartinstall.Installer and injected via WithPIIRampartInstaller.
type piiRampartInstaller interface {
	Start(updatedBy string) error
	Status(ctx context.Context) rampartinstall.Status
	Uninstall(ctx context.Context) error
}

// WithPIIRampartInstaller injects the installer. Omitted (tests, mock mode,
// or deployments that manage the service themselves), the endpoints answer
// 501 and the panel hides the install affordance.
func WithPIIRampartInstaller(inst piiRampartInstaller) Option {
	return func(s *Server) { s.piiInstaller = inst }
}

// handleAdminPIIInstall serves /admin/pii-redaction/install:
// GET = job status + container state, POST = start the install,
// DELETE = remove the managed container.
func (s *Server) handleAdminPIIInstall(w http.ResponseWriter, r *http.Request) {
	if s.piiInstaller == nil {
		http.Error(w, "rampart installer unavailable", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.piiInstaller.Status(r.Context()))
	case http.MethodPost:
		if err := s.piiInstaller.Start(userFromCtx(r.Context())); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, s.piiInstaller.Status(r.Context()))
	case http.MethodDelete:
		if err := s.piiInstaller.Uninstall(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.piiInstaller.Status(r.Context()))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
