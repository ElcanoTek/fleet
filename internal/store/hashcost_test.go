package store

import "golang.org/x/crypto/bcrypt"

// The suite provisions users by the hundred; at bcrypt.DefaultCost the hashing
// alone is ~70ms per user (far more under -race) and swamps the database work
// these tests exist to exercise. Nothing here asserts on hash strength.
func init() {
	SetPasswordHashCostForTests(bcrypt.MinCost)
}
