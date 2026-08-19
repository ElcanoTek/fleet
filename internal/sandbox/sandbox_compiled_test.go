package sandbox

import "testing"

func TestHostExecutorCompiledIn(t *testing.T) {
	if got := HostExecutorCompiledIn(); got != hostExecutorCompiledIn {
		t.Errorf("HostExecutorCompiledIn() = %v, want %v", got, hostExecutorCompiledIn)
	}
}
