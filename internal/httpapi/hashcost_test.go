package httpapi

import (
	"golang.org/x/crypto/bcrypt"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Every fixture user costs a bcrypt hash; at the default work factor that is
// ~70ms of CPU per user (far more under -race) on tests that are about HTTP
// handlers, not password strength. Lowered for the whole test binary.
func init() {
	store.SetPasswordHashCostForTests(bcrypt.MinCost)
}
