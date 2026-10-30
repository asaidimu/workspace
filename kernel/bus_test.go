package kernel

import (
	"context"
	"testing"
	"time"
)

func testEnv(name, owner, conv string) EventEnvelope {
	return EventEnvelope{
		Name:           name,
		Owner:          owner,
		ConversationID: conv,
		At:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Event:          thingCreated{ID: conv},
	}
}

func TestBusDeliversToMatchingSubscribersOnly(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	all, err := b.Subscribe(ctx, EventFilter{}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ownerOnly, err := b.Subscribe(ctx, EventFilter{Owner: "u1"}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	convOnly, err := b.Subscribe(ctx, EventFilter{ConversationID: "conv-1"}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	named, err := b.Subscribe(ctx, EventFilter{Names: []string{"thing.created"}}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	b.publish(testEnv("thing.created", "u1", "conv-1"))
	b.publish(testEnv("thing.deleted", "u2", "conv-2"))

	if env := <-all; env.Name != "thing.created" {
		t.Fatalf("all subscriber first = %q, want thing.created", env.Name)
	}
	if env := <-all; env.Name != "thing.deleted" {
		t.Fatalf("all subscriber second = %q, want thing.deleted", env.Name)
	}

	if env := <-ownerOnly; env.Owner != "u1" || env.Name != "thing.created" {
		t.Fatalf("ownerOnly got %+v, want u1's event only", env)
	}
	select {
	case env := <-ownerOnly:
		t.Fatalf("ownerOnly saw another owner's event: %+v", env)
	default:
	}

	if env := <-convOnly; env.ConversationID != "conv-1" {
		t.Fatalf("convOnly got conversation %q", env.ConversationID)
	}
	select {
	case env := <-convOnly:
		t.Fatalf("convOnly saw another conversation: %+v", env)
	default:
	}

	if env := <-named; env.Name != "thing.created" {
		t.Fatalf("named got %q", env.Name)
	}
	select {
	case env := <-named:
		t.Fatalf("named saw a name it did not ask for: %+v", env)
	default:
	}
}

func TestBusDeliversInOrder(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := b.Subscribe(ctx, EventFilter{ConversationID: "conv-1"}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i, name := range []string{"a", "b", "c"} {
		b.publish(testEnv(name, "u1", "conv-1"))
		if env := <-sub; env.Name != name {
			t.Fatalf("event %d = %q, want %q", i, env.Name, name)
		}
	}
}

func TestBusOverflowClosesSlowSubscriber(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A buffer of one plus three publishes forces an overflow on the third.
	sub, err := b.Subscribe(ctx, EventFilter{}, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	b.publish(testEnv("a", "u1", "conv-1"))
	b.publish(testEnv("b", "u1", "conv-1"))
	b.publish(testEnv("c", "u1", "conv-1"))

	env, ok := <-sub
	if !ok {
		t.Fatal("channel closed without an Overflow signal")
	}
	if _, isOverflow := env.Event.(Overflow); !isOverflow || env.Name != "workspace.overflow" {
		t.Fatalf("last delivery = %+v, want an Overflow event", env)
	}
	if _, ok := <-sub; ok {
		t.Fatal("channel must close after an Overflow")
	}
	// A later publish for a closed subscriber must not panic.
	b.publish(testEnv("d", "u1", "conv-1"))
}

func TestBusUnsubscribesOnContextCancel(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := b.Subscribe(ctx, EventFilter{}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	waitUntil(t, "the subscription to register", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.subs) == 1
	})

	cancel()
	if _, ok := <-sub; ok {
		t.Fatal("channel must close when the context ends")
	}
	waitUntil(t, "the subscription to be removed", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.subs) == 0
	})
	// Closing again must be harmless.
	b.close(0)
}

func TestBusSubscribeRejectsCancelledContext(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Subscribe(ctx, EventFilter{}, 8); !IsCode(err, CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
}

func TestBusUsesDefaultBufferForNonPositiveSize(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := b.Subscribe(ctx, EventFilter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := cap(sub); got != defaultEventBuffer {
		t.Fatalf("cap = %d, want the default %d", got, defaultEventBuffer)
	}
}

func TestBusPublishWithNoSubscribersIsHarmless(t *testing.T) {
	b := newBus()
	b.publish(testEnv("a", "u1", "conv-1")) // must not panic
}

func TestBusOverflowEventName(t *testing.T) {
	if got := (Overflow{}).EventName(); got != "workspace.overflow" {
		t.Fatalf("EventName = %q", got)
	}
}

func TestContainsName(t *testing.T) {
	if !containsName([]string{"a", "b"}, "b") {
		t.Fatal("containsName missed a present name")
	}
	if containsName([]string{"a", "b"}, "c") {
		t.Fatal("containsName matched an absent name")
	}
	if containsName(nil, "a") {
		t.Fatal("containsName matched an empty list")
	}
}

func TestConversationOf(t *testing.T) {
	if got := conversationOf(createThing{}); got != "conv-1" {
		t.Fatalf("resource command conversation = %q, want conv-1", got)
	}
	if got := conversationOf(ownerThing{}); got != "" {
		t.Fatalf("owner-level command conversation = %q, want empty", got)
	}
}
