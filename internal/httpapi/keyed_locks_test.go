package httpapi

import (
	"testing"
	"time"
)

// keyedLocks serializes one key, leaves other keys alone, and drops a key's
// entry once nobody holds or waits on it.
func TestKeyedLocks(t *testing.T) {
	var k keyedLocks
	unlockA := k.lock("a")
	unlockB := k.lock("b") // another key is not blocked
	unlockB()

	acquired := make(chan func(), 1)
	go func() { acquired <- k.lock("a") }()
	select {
	case <-acquired:
		t.Fatal("the same key was locked twice")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case u := <-acquired:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the key stayed locked after unlock")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.m) != 0 {
		t.Fatalf("entries left behind: %d", len(k.m))
	}
}
