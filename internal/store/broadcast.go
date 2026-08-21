package store

import "sync"

// broadcast wakes every waiting long-poller when an event is appended — the
// in-process wakeup channel for the SQLite backend (Postgres uses
// LISTEN/NOTIFY, M9).
type broadcast struct {
	mu sync.Mutex
	ch chan struct{}
}

func newBroadcast() *broadcast {
	return &broadcast{ch: make(chan struct{})}
}

// wait returns a channel that is closed at the next notify.
func (b *broadcast) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ch
}

func (b *broadcast) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.ch)
	b.ch = make(chan struct{})
}
