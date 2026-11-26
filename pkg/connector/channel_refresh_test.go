package connector

// refreshes returns the queued power-level refreshes, in FIFO order.
func (r *eventRecorder) refreshes() []*mmChannelRefreshEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*mmChannelRefreshEvent
	for _, ev := range r.events {
		if refresh, ok := ev.(*mmChannelRefreshEvent); ok {
			out = append(out, refresh)
		}
	}
	return out
}
