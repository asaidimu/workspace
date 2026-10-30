package kernel

import (
	"context"
	"sync"
)

// serializer admits commands FIFO per key, where the key is the conversation
// a command targets, or the owner for owner-level commands (Workspace
// specification, section 7).
//
// It is a correctness aid, not the source of it: it stops two commands for
// the same conversation from interleaving their read-modify-write on one
// goroutine pair, but cross-process correctness comes from the store's
// compare-and-swap. The lock is never held across a provider call, because a
// command's transaction is closed before the agent loop reaches the provider.
//
// Locks are keyed per conversation and are deliberately never removed from
// the map. A lock that is deleted and recreated can hand two goroutines two
// independent locks for the same key, which silently breaks mutual
// exclusion; a refcount scheme avoids that but costs more than the memory it
// saves, since the map holds one small struct per conversation ever used.
type serializer struct {
	mu    sync.Mutex
	locks map[string]*keyedMutex
}

func newSerializer() *serializer {
	return &serializer{locks: map[string]*keyedMutex{}}
}

// keyedMutex is one key's lock: a held flag plus the waiters queued FIFO.
type keyedMutex struct {
	mu     sync.Mutex
	held   bool
	waiter []chan struct{}
}

// lock acquires the key, waiting in FIFO order. It honours the context while
// waiting, and removes the caller from the queue on cancellation so a later
// holder is not signalled into a channel nobody reads.
func (s *serializer) lock(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return wrapContext(err)
	}

	s.mu.Lock()
	km := s.locks[key]
	if km == nil {
		km = &keyedMutex{}
		s.locks[key] = km
	}
	s.mu.Unlock()

	km.mu.Lock()
	if !km.held {
		km.held = true
		km.mu.Unlock()
		return nil
	}

	ch := make(chan struct{}, 1)
	km.waiter = append(km.waiter, ch)
	km.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		km.mu.Lock()
		for i, w := range km.waiter {
			if w == ch {
				km.waiter = append(km.waiter[:i], km.waiter[i+1:]...)
				break
			}
		}
		km.mu.Unlock()
		return wrapContext(ctx.Err())
	}
}

// unlock releases the key, handing it to the longest waiter if one is queued.
// The held flag stays true across a handoff, so only one holder ever exists.
func (s *serializer) unlock(key string) {
	s.mu.Lock()
	km := s.locks[key]
	s.mu.Unlock()
	if km == nil {
		return
	}

	km.mu.Lock()
	if len(km.waiter) > 0 {
		next := km.waiter[0]
		km.waiter = km.waiter[1:]
		km.mu.Unlock()
		next <- struct{}{}
		return
	}
	km.held = false
	km.mu.Unlock()
}

// serializerKey is the FIFO key for a command: the resource it targets, so
// two commands for different resources never block each other, or the owner
// when the command is owner-level.
func serializerKey(p Principal, t Target) string {
	if t.Kind != "" && t.ID != "" {
		return t.Kind + "/" + t.ID
	}
	return "owner/" + p.Owner
}
