package store

import (
	"testing"
	"time"
)

// TestOpen_AppliesPoolConfig proves the PoolConfig passed to Open is actually
// applied to the underlying *sql.DB (#276), not silently ignored.
func TestOpen_AppliesPoolConfig(t *testing.T) {
	t.Parallel()

	dsn := newTestDSN(t)
	s, err := Open(dsn, PoolConfig{
		MaxOpenConns:    7,
		MaxIdleConns:    2,
		ConnMaxIdleTime: 90 * time.Second,
		ConnMaxLifetime: time.Minute,
		ConnectTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := s.PoolStats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections: got %d, want 7", got)
	}
}
