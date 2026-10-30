package kernel

import (
	"context"
	"sync"
)

// @note #hnpcoz todo : Replace this bus with a more robust implementation
//
// This bus suffices for now, but we should consider using a better version

// defaultEventBuffer is the subscriber buffer size when a caller passes a
// non-positive one (Workspace specification, section 15).
const defaultEventBuffer = 64

// Overflow is published to a subscriber that could not keep up, immediately
// before its channel is closed. It is the one signal a subscriber gets that
// it has missed events: the buffered deliveries are discarded, so the
// subscriber must re-sync by querying the store.
type Overflow struct{}

// EventName implements Event.
func (Overflow) EventName() string { return "workspace.overflow" }

// bus is the in-process event bus. Delivery is ordered per conversation and
// at most once: publish is called synchronously after a commit, under the
// bus lock, so events for one conversation reach every matching subscriber
// in the order they were committed.
type bus struct {
	mu   sync.Mutex
	subs map[uint64]*subscriber
	next uint64
}

func newBus() *bus {
	return &bus{subs: map[uint64]*subscriber{}}
}

type subscriber struct {
	id     uint64
	filter EventFilter
	ch     chan EventEnvelope
	closed bool
}

// matches reports whether the subscriber wants this event. An empty filter
// field matches everything; callers should set Owner so a subscription can
// never observe another owner's events by accident.
func (s *subscriber) matches(env EventEnvelope) bool {
	if s.filter.Owner != "" && s.filter.Owner != env.Owner {
		return false
	}
	if s.filter.ConversationID != "" && s.filter.ConversationID != env.ConversationID {
		return false
	}
	if len(s.filter.Names) > 0 && !containsName(s.filter.Names, env.Name) {
		return false
	}
	return true
}

// Subscribe returns a channel of committed events matching the filter. The
// channel closes when the context ends or when the subscriber overflows; a
// caller that stops reading must cancel its context to unsubscribe.
func (b *bus) Subscribe(ctx context.Context, f EventFilter, buffer int) (<-chan EventEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrapContext(err)
	}
	if buffer <= 0 {
		buffer = defaultEventBuffer
	}

	s := &subscriber{id: b.next, filter: f, ch: make(chan EventEnvelope, buffer)}
	b.mu.Lock()
	b.next++
	b.subs[s.id] = s
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.close(s.id)
	}()
	return s.ch, nil
}

// publish delivers one event to every matching subscriber. It runs under the
// bus lock, so a subscriber's channel can never be closed while an event is
// being offered to it.
func (b *bus) publish(env EventEnvelope) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, s := range b.subs {
		if !s.matches(env) {
			continue
		}
		select {
		case s.ch <- env:
		default:
			// The subscriber is too slow. Discard what it could not keep up
			// with, deliver a final Overflow so it knows to re-sync by
			// querying, and close its channel.
			drain(s.ch)
			s.ch <- EventEnvelope{
				Name:          Overflow{}.EventName(),
				Owner:         env.Owner,
				CorrelationID: env.CorrelationID,
				At:            env.At,
				Event:         Overflow{},
			}
			b.closeLocked(id)
		}
	}
}

// close unsubscribes one subscriber and closes its channel, so a reader
// blocked on receive wakes up. It is safe to call twice.
func (b *bus) close(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked(id)
}

func (b *bus) closeLocked(id uint64) {
	s, ok := b.subs[id]
	if !ok {
		return
	}
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	delete(b.subs, id)
}

// drain empties a channel so one more event fits. The discarded events are
// the ones the subscriber lost; it will re-sync by querying.
func drain(ch chan EventEnvelope) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// conversationOf returns the conversation a command targets, which is the
// ordering and filtering key for its events. Owner-level commands carry an
// empty conversation ID.
func conversationOf(c Command) string {
	t := targetOf(c)
	if t.Kind == "conversation" {
		return t.ID
	}
	return ""
}

// Targetable is implemented by a command that acts on a specific resource.
// Commands that do not implement it are owner-level: they are serialized
// against their owner rather than against a resource, and their events carry
// no conversation ID.
type Targetable interface {
	Target() Target
}

// targetOf returns a command's target, or the zero Target when it is
// owner-level.
func targetOf(c Command) Target {
	if t, ok := c.(Targetable); ok {
		return t.Target()
	}
	return Target{}
}
