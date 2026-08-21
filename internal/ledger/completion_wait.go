package ledger

import (
	"context"
	"sync"
	"time"
)

// completionWaiters turns completion polling into a durable-check plus an
// in-process notification. The database remains authoritative across restarts;
// notifications only eliminate repeated status queries while work is active.
type completionWaiters struct {
	mu      sync.Mutex
	nextID  uint64
	waiters map[string]map[uint64]chan struct{}
}

func newCompletionWaiters() *completionWaiters {
	return &completionWaiters{waiters: make(map[string]map[uint64]chan struct{})}
}

func (w *completionWaiters) subscribe(key string) (<-chan struct{}, func()) {
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	channel := make(chan struct{})
	if w.waiters[key] == nil {
		w.waiters[key] = make(map[uint64]chan struct{})
	}
	w.waiters[key][id] = channel
	w.mu.Unlock()
	return channel, func() {
		w.mu.Lock()
		if subscribers := w.waiters[key]; subscribers != nil {
			delete(subscribers, id)
			if len(subscribers) == 0 {
				delete(w.waiters, key)
			}
		}
		w.mu.Unlock()
	}
}

func (w *completionWaiters) notify(key string) {
	w.mu.Lock()
	subscribers := w.waiters[key]
	delete(w.waiters, key)
	for _, channel := range subscribers {
		close(channel)
	}
	w.mu.Unlock()
}

func waitForCompletion[T any](ctx context.Context, timeout time.Duration, read func() (T, bool, error), waiters *completionWaiters, key string) (T, error) {
	value, complete, err := read()
	if err != nil || complete || timeout <= 0 {
		return value, err
	}
	channel, unsubscribe := waiters.subscribe(key)
	defer unsubscribe()
	// Close the read/subscribe race before blocking.
	value, complete, err = read()
	if err != nil || complete {
		return value, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-channel:
	case <-waitCtx.Done():
	}
	value, _, err = read()
	return value, err
}
