package reclaim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettled(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{
		{0, false},
		{Grace - time.Minute, false},
		{Grace, true},
		{48 * time.Hour, true},
		{-time.Minute, false},              // a little ahead (clock skew): still new
		{-Grace, false},                    // up to Grace ahead: still new
		{-(Grace + time.Minute), true},     // further ahead: not a creation time
		{-75 * 365 * 24 * time.Hour, true}, // touch -d 2099
	} {
		mtime := now.Add(-tc.age)
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := Settled(info, now); got != tc.want {
			t.Errorf("modified %v ago: Settled = %v, want %v", tc.age, got, tc.want)
		}
	}
}

func TestLocks(t *testing.T) {
	t.Parallel()
	var l Locks

	unlock := l.Lock("tasks/a")
	if _, ok := l.TryLock("tasks/a"); ok {
		t.Fatal("TryLock took a held key")
	}
	other, ok := l.TryLock("tasks/b")
	if !ok {
		t.Fatal("TryLock refused a free key")
	}
	other()

	got := make(chan struct{})
	go func() {
		defer close(got)
		l.Lock("tasks/a")()
	}()
	unlock()
	<-got

	again, ok := l.TryLock("tasks/a")
	if !ok {
		t.Fatal("a released key is still held")
	}
	again()
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("%d keys left with nobody holding or waiting on them", n)
	}
}
