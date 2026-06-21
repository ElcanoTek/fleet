package sandbox

import "testing"

// TestCappedBuffer pins the bounded output-capture behavior folded in
// from cutlass's direct-exec bash path: the buffer stores at most cap
// bytes, discards (and counts) the rest, and always reports a
// full-length write so exec's pipe copier never errors.
func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{cap: 8}
	n, err := c.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	// Crosses the cap: stores 3, discards 4, but reports full length so the
	// writer (exec's pipe copier) never errors.
	n, err = c.Write([]byte("6789abc"))
	if err != nil || n != 7 {
		t.Fatalf("capped write: n=%d err=%v", n, err)
	}
	if got := c.buf.String(); got != "12345678" {
		t.Fatalf("stored bytes = %q, want %q", got, "12345678")
	}
	if c.discarded != 4 {
		t.Fatalf("discarded = %d, want 4", c.discarded)
	}
	// Fully past the cap: everything discarded.
	if n, err := c.Write([]byte("zz")); err != nil || n != 2 {
		t.Fatalf("past-cap write: n=%d err=%v", n, err)
	}
	if c.discarded != 6 {
		t.Fatalf("discarded = %d, want 6", c.discarded)
	}
}
