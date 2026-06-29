package sandbox

import "testing"

// TestResourceOverride_applyTo is the #205 unit test the issue asks for: a
// ContainerConfig with explicit per-task limits carries the expected
// MemoryLimit / CPULimit / PidsLimit (which containerImpl.start emits verbatim as
// --memory / --memory-swap / --cpus / --pids-limit). A zero/empty override field
// leaves the pool default untouched.
func TestResourceOverride_applyTo(t *testing.T) {
	base := ContainerConfig{MemoryLimit: "512m", CPULimit: "1.0", PidsLimit: 128}

	// All three overridden.
	got := ResourceOverride{MemoryLimit: "2048m", CPULimit: "2.00", PidsLimit: 512}.applyTo(base)
	if got.MemoryLimit != "2048m" || got.CPULimit != "2.00" || got.PidsLimit != 512 {
		t.Errorf("full override = {%s %s %d}, want {2048m 2.00 512}", got.MemoryLimit, got.CPULimit, got.PidsLimit)
	}

	// Partial override leaves the unset fields at the base default.
	got = ResourceOverride{MemoryLimit: "4096m"}.applyTo(base)
	if got.MemoryLimit != "4096m" || got.CPULimit != "1.0" || got.PidsLimit != 128 {
		t.Errorf("partial override = {%s %s %d}, want {4096m 1.0 128}", got.MemoryLimit, got.CPULimit, got.PidsLimit)
	}

	// Empty override is a no-op (compare the three relevant fields; ContainerConfig
	// has slice fields that aren't ==-comparable).
	got = (ResourceOverride{}).applyTo(base)
	if got.MemoryLimit != base.MemoryLimit || got.CPULimit != base.CPULimit || got.PidsLimit != base.PidsLimit {
		t.Errorf("empty override changed limits: {%s %s %d}", got.MemoryLimit, got.CPULimit, got.PidsLimit)
	}

	// The base is not mutated (value semantics).
	if base.MemoryLimit != "512m" || base.PidsLimit != 128 {
		t.Errorf("base was mutated: %+v", base)
	}
}
