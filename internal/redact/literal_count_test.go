package redact

// LiteralCount reports how many literal VALUES are currently scanned for. It
// exposes the count only, never a value — it is the diagnostic that lets the
// rotation tests assert the scan set plateaus (#1274).
func (r *Redactor) LiteralCount() int {
	if r == nil {
		return 0
	}
	r.sweepDue()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.literals)
}
