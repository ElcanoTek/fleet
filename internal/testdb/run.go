package testdb

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run runs a package's tests with testdb in background mode, from the
// package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
//
// Creating and dropping a database costs a few hundred milliseconds of server
// time each. In background mode that cost moves off the tests' critical path:
// clones of each template a test has asked for are made ahead, a few at a
// time (up to GOMAXPROCS waiting, never more than the package has used so
// far), and a finished test's database is dropped by a background dropper
// instead of in its cleanup. Run returns m.Run's code only after every
// background drop has finished and every clone made ahead but never used has
// been dropped, so a completed run leaves no fleet_t_ database behind. A
// killed run's leftovers are garbage-collected by the next one (see the
// package comment).
//
// Without Run (a package with no TestMain, or one that does not call it)
// everything still works: each clone is made and dropped synchronously.
func Run(m *testing.M) int {
	runMode.Store(true)
	code := m.Run()
	if s := live.Load(); s != nil {
		s.shutdown()
	}
	return code
}

var (
	runMode atomic.Bool            // Run is driving this process
	live    atomic.Pointer[server] // the server, once connected
)

// pool holds clones of one template made ahead of the tests that need them.
type pool struct {
	ready chan string   // clones waiting for a test
	kick  chan struct{} // a test took one (or found none): consider refilling
	takes atomic.Int64  // how many tests have asked so far
}

// background holds the state of background mode on a server.
type background struct {
	on      atomic.Bool
	mu      sync.Mutex
	pools   map[string]*pool
	stop    chan struct{}
	fillers sync.WaitGroup
	drops   chan string
	pending sync.WaitGroup
}

// startBackground turns background mode on, when Run drives the process.
func (s *server) startBackground() {
	if !runMode.Load() {
		return
	}
	b := &s.bg
	b.pools = map[string]*pool{}
	b.stop = make(chan struct{})
	// A short queue is the backpressure that bounds how many finished tests'
	// databases exist at once (disk): when the droppers fall behind, a test's
	// cleanup waits for a slot instead of piling up databases.
	n := runtime.GOMAXPROCS(0)
	b.drops = make(chan string, n)
	for range n {
		go func() {
			for name := range b.drops {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				if err := s.drop(ctx, name); err != nil {
					fmt.Fprintf(os.Stderr, "testdb: background drop: %v\n", err)
				}
				cancel()
				b.pending.Done()
			}
		}()
	}
	b.on.Store(true)
}

// dropLater hands name to the background dropper.
func (s *server) dropLater(name string) {
	s.bg.pending.Add(1)
	s.bg.drops <- name
}

// pooled takes a ready clone of template, if one is waiting. It also starts
// the template's filler on first use and asks it to top the pool up.
func (s *server) pooled(template string) (string, bool) {
	b := &s.bg
	if !b.on.Load() {
		return "", false
	}
	b.mu.Lock()
	p := b.pools[template]
	if p == nil {
		p = &pool{ready: make(chan string, runtime.GOMAXPROCS(0)), kick: make(chan struct{}, 1)}
		b.pools[template] = p
		b.fillers.Add(1)
		go s.fill(template, p)
	}
	b.mu.Unlock()
	p.takes.Add(1)
	defer func() {
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}()
	select {
	case name := <-p.ready:
		return name, true
	default:
		return "", false
	}
}

// fill keeps p topped up: up to its capacity, and never more clones waiting
// than tests have asked for so far (a package with two DB tests does not get
// GOMAXPROCS clones made for nothing). One filler per template; a test that
// finds the pool empty makes its own clone meanwhile.
func (s *server) fill(template string, p *pool) {
	b := &s.bg
	defer b.fillers.Done()
	for {
		select {
		case <-b.stop:
			return
		case <-p.kick:
		}
		for int64(len(p.ready)) < min(int64(cap(p.ready)), p.takes.Load()) {
			select {
			case <-b.stop:
				return
			default:
			}
			name := s.newName()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := s.createDB(ctx, name, template)
			cancel()
			if err != nil {
				// The template went away or the server is struggling: stop
				// filling; tests make their own clones and report the error.
				s.dropLater(name)
				return
			}
			p.ready <- name // cannot block: this is the only sender, below capacity
		}
	}
}

// shutdown stops the fillers, drops every clone nobody used, and waits for
// every background drop.
func (s *server) shutdown() {
	b := &s.bg
	if !b.on.Load() {
		return
	}
	close(b.stop)
	b.fillers.Wait()
	b.mu.Lock()
	for _, p := range b.pools {
		for len(p.ready) > 0 {
			s.dropLater(<-p.ready)
		}
	}
	b.mu.Unlock()
	b.pending.Wait()
}
