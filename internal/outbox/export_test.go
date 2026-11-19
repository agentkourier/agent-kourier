package outbox

// Untried is how many rows the Outbox holds as written by its own Enqueue and not yet tried, with the writes of Enqueue still
// in flight: a test sees with it that every way a row leaves the backlog takes it out of the set.
func (o *Outbox) Untried() int {
	o.untriedMu.Lock()
	defer o.untriedMu.Unlock()
	return len(o.untried) + len(o.writing)
}
