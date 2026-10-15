package interact

// TakenCount is how many denials the session took that the sweeper still remembers, for a test of when it forgets them.
func (s *Sweeper) TakenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.taken)
}

// UndeliveredCount is how many denials that were not delivered the sweeper still has to deal with.
func (s *Sweeper) UndeliveredCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.undelivered)
}

// Len is how many pauses the watcher holds for the sweeper.
func (r *Reopened) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}
