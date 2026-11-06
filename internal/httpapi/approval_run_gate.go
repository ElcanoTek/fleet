package httpapi

import (
	"context"
	"strings"
	"sync"
)

// DrainApprovalRuns stops admitting approved executions and blocks until
// every one already admitted has recorded its outcome, or ctx (the shutdown
// grace) fires; it reports which. From its first call on, a new approve POST
// is refused before its claim, so the card stays pending. An execution cut
// off by the process exit leaves its sentinel standing, and the next boot's
// RecoverStrandedApprovals records it as outcome unknown.
func (s *Server) DrainApprovalRuns(ctx context.Context) bool {
	return s.approvalRuns.drain(ctx)
}

type executingApprovalEntry struct {
	convID     string
	userEmail  string
	approvalID string
	ctx        context.Context
	cancel     context.CancelFunc
}

type executingApprovalRegistry struct {
	mu sync.Mutex
	m  map[string]executingApprovalEntry
}

func (r *executingApprovalRegistry) register(parent context.Context, convID, userEmail, approvalID string) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]executingApprovalEntry)
	}
	if existing, ok := r.m[approvalID]; ok && existing.ctx != nil {
		return existing.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	r.m[approvalID] = executingApprovalEntry{
		convID:     strings.TrimSpace(convID),
		userEmail:  strings.ToLower(strings.TrimSpace(userEmail)),
		approvalID: approvalID,
		ctx:        ctx,
		cancel:     cancel,
	}
	return ctx
}

func (r *executingApprovalRegistry) unregister(approvalID string) {
	r.mu.Lock()
	entry, ok := r.m[approvalID]
	if ok {
		delete(r.m, approvalID)
	}
	r.mu.Unlock()
	if ok && entry.cancel != nil {
		entry.cancel()
	}
}

func (r *executingApprovalRegistry) convIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.m {
		if e.convID != "" {
			out = append(out, e.convID)
		}
	}
	return out
}

func (r *executingApprovalRegistry) cancelUnauthorized(convID, ownerEmail string, allowed map[string]bool, stops *approvalStopRegistry) []string {
	ownerKey := strings.ToLower(strings.TrimSpace(ownerEmail))
	r.mu.Lock()
	var toCancel []executingApprovalEntry
	for _, e := range r.m {
		if convID != "" && e.convID != convID {
			continue
		}
		if e.userEmail == "" || e.userEmail == ownerKey || allowed[e.userEmail] {
			continue
		}
		toCancel = append(toCancel, e)
	}
	r.mu.Unlock()
	senders := make([]string, 0, len(toCancel))
	for _, e := range toCancel {
		senders = append(senders, e.userEmail)
		if stops != nil {
			stops.mu.Lock()
			h := stops.m[e.approvalID]
			stops.mu.Unlock()
			if h != nil {
				h.stop()
			}
		}
		if e.cancel != nil {
			e.cancel()
		}
	}
	return senders
}

// approvalRunGate admits and counts approved executions for the shutdown
// drain. A sync.WaitGroup cannot do this job: an Add that races a Wait which
// has already seen zero breaks its contract, and the approve route stays
// reachable while the server drains. Admission and drain share one mutex, so
// once drain has started no execution can begin. The zero value is ready.
type approvalRunGate struct {
	mu       sync.Mutex
	active   int
	draining bool
	idle     chan struct{} // closed when draining and active reaches zero
}

// enter admits one execution, or refuses it once a drain has begun.
// Increment active before checking the filesystem drain sentinel so any updater
// that sets the sentinel after enter() succeeds is guaranteed to observe
// active > 0 on its subsequent /healthz probe.
func (g *approvalRunGate) enter() bool {
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return false
	}
	g.active++
	g.mu.Unlock()
	if UpdateDrainActive() {
		g.leave()
		return false
	}
	return true
}

// leave ends one admitted execution.
func (g *approvalRunGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.draining && g.active == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

func (g *approvalRunGate) activeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// drain refuses further admissions and waits for the admitted ones, or ctx.
func (g *approvalRunGate) drain(ctx context.Context) bool {
	g.mu.Lock()
	g.draining = true
	if g.active == 0 {
		g.mu.Unlock()
		return true
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
}
