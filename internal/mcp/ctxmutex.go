package mcp

import (
	"context"
	"sync"
)

// ctxMutex is a mutex whose waiters can give up. A waiter is a blocked
// channel send, not a goroutine, so a cancelled one simply leaves the queue:
// calls queued behind a slow one and then cancelled leave nothing behind,
// however many there are. The zero value is unlocked.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ctxMutex) slot() chan struct{} {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	return m.ch
}

func (m *ctxMutex) Lock() { m.slot() <- struct{}{} }

func (m *ctxMutex) TryLock() bool {
	select {
	case m.slot() <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *ctxMutex) Unlock() {
	select {
	case <-m.slot():
	default:
		panic("mcp: unlock of unlocked ctxMutex")
	}
}

// lockWithContext takes mu, or gives up when ctx ends first (returning its
// error and holding nothing).
func lockWithContext(ctx context.Context, mu *ctxMutex) error {
	select {
	case mu.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
