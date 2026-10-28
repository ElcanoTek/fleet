package agentcore

import "testing"

// The tier holders back the admin default_model/advanced_model settings
// (#1187): seeded from the compiled-in constants, swapped live by the
// cmd/fleet apply hooks, and never blankable — "" reverts to the constant so
// an unset env default can't erase a tier.
func TestModelTierHolders(t *testing.T) {
	t.Cleanup(func() {
		SetDefaultModel("")
		SetAdvancedModel("")
	})

	if got := CurrentDefaultModel(); got != DefaultCoreModel {
		t.Fatalf("seed default = %q, want DefaultCoreModel %q", got, DefaultCoreModel)
	}
	if got := CurrentAdvancedModel(); got != DefaultMaxModel {
		t.Fatalf("seed advanced = %q, want DefaultMaxModel %q", got, DefaultMaxModel)
	}

	SetDefaultModel("acme/frontier-1")
	SetAdvancedModel("acme/frontier-1-pro")
	if got := CurrentDefaultModel(); got != "acme/frontier-1" {
		t.Fatalf("default after set = %q", got)
	}
	if got := CurrentAdvancedModel(); got != "acme/frontier-1-pro" {
		t.Fatalf("advanced after set = %q", got)
	}

	// Whitespace-only or empty reverts to the compiled-in seed.
	SetDefaultModel("   ")
	SetAdvancedModel("")
	if got := CurrentDefaultModel(); got != DefaultCoreModel {
		t.Fatalf("default after clear = %q, want %q", got, DefaultCoreModel)
	}
	if got := CurrentAdvancedModel(); got != DefaultMaxModel {
		t.Fatalf("advanced after clear = %q, want %q", got, DefaultMaxModel)
	}
}

// The task tier has no seed of its own: unset, it follows the default frontier
// tier live, so moving the default model moves scheduled work with it. An
// override pins it independently, and clearing the override restores the follow.
func TestTaskModelTierFollowsDefault(t *testing.T) {
	t.Cleanup(func() {
		SetDefaultModel("")
		SetTaskModel("")
	})

	if got := CurrentTaskModel(); got != DefaultCoreModel {
		t.Fatalf("seed task tier = %q, want the default seed %q", got, DefaultCoreModel)
	}

	SetDefaultModel("acme/frontier-1-pro")
	if got := CurrentTaskModel(); got != "acme/frontier-1-pro" {
		t.Fatalf("task tier after a default change = %q, want it to follow", got)
	}

	SetTaskModel("  acme/long-runner  ")
	if got := CurrentTaskModel(); got != "acme/long-runner" {
		t.Fatalf("task tier with an override = %q", got)
	}
	SetDefaultModel("acme/frontier-2-pro")
	if got := CurrentTaskModel(); got != "acme/long-runner" {
		t.Fatalf("an override must not follow the default tier, got %q", got)
	}

	SetTaskModel("")
	if got := CurrentTaskModel(); got != "acme/frontier-2-pro" {
		t.Fatalf("task tier after clearing the override = %q, want the default tier", got)
	}
}
