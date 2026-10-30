package workspacetest

import (
	"context"
	"sync"

	"github.com/asaidimu/workspace/kernel"
)

// EventLog records the events a workspace published, and lets a test wait
// for one by name instead of sleeping. Waiting on an event is the rule
// (Workspace specification, section 16); sleeping is not.
type EventLog struct {
	mu      sync.Mutex
	all     []kernel.EventEnvelope
	waiters []waiter
}

type waiter struct {
	name string
	ch   chan kernel.EventEnvelope
}

// NewEventLog returns an empty log.
func NewEventLog() *EventLog { return &EventLog{} }

// Record stores an event and wakes anyone waiting for its name. It is what
// the harness's bus subscription calls.
func (l *EventLog) Record(env kernel.EventEnvelope) {
	l.mu.Lock()
	l.all = append(l.all, env)
	var ready []waiter
	kept := l.waiters[:0]
	for _, w := range l.waiters {
		if w.name == env.Name {
			ready = append(ready, w)
		} else {
			kept = append(kept, w)
		}
	}
	l.waiters = kept
	l.mu.Unlock()

	for _, w := range ready {
		w.ch <- env
	}
}

// WaitFor blocks until an event with the given name has been recorded and
// returns it. It does not consume the event: All still reports everything
// that was published, which is what an assertion on the full sequence needs.
// It returns ctx.Err() if the context ends first, so a hung publish fails
// the test rather than stalling it.
func (l *EventLog) WaitFor(ctx context.Context, name string) (kernel.EventEnvelope, error) {
	l.mu.Lock()
	for _, env := range l.all {
		if env.Name == name {
			l.mu.Unlock()
			return env, nil
		}
	}
	w := waiter{name: name, ch: make(chan kernel.EventEnvelope, 1)}
	l.waiters = append(l.waiters, w)
	l.mu.Unlock()

	select {
	case env := <-w.ch:
		return env, nil
	case <-ctx.Done():
		return kernel.EventEnvelope{}, ctx.Err()
	}
}

// All returns every event recorded so far, in order.
func (l *EventLog) All() []kernel.EventEnvelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]kernel.EventEnvelope, len(l.all))
	copy(out, l.all)
	return out
}
