package kernel

import (
	"context"
	"errors"
	"testing"
)

func waiterCount(s *serializer, key string) int {
	s.mu.Lock()
	km := s.locks[key]
	s.mu.Unlock()
	if km == nil {
		return 0
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	return len(km.waiter)
}

func TestSerializerMutualExclusion(t *testing.T) {
	s := newSerializer()
	ctx := context.Background()
	if err := s.lock(ctx, "conv-1"); err != nil {
		t.Fatalf("first lock: %v", err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- s.lock(ctx, "conv-1") }()

	// The second lock must not complete while the first is held. The
	// condition wait observes the queued waiter instead of sleeping.
	waitUntil(t, "a queued waiter", func() bool { return waiterCount(s, "conv-1") == 1 })
	select {
	case err := <-acquired:
		t.Fatalf("second lock returned %v while the first was held", err)
	default:
	}

	s.unlock("conv-1")
	if err := <-acquired; err != nil {
		t.Fatalf("second lock after release: %v", err)
	}
	s.unlock("conv-1")
}

func TestSerializerDifferentKeysDoNotBlock(t *testing.T) {
	s := newSerializer()
	ctx := context.Background()
	if err := s.lock(ctx, "conv-1"); err != nil {
		t.Fatalf("lock conv-1: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.lock(ctx, "conv-2") }()
	if err := <-done; err != nil {
		t.Fatalf("lock conv-2 must not wait for conv-1: %v", err)
	}
	s.unlock("conv-2")
	s.unlock("conv-1")
}

func TestSerializerHonoursContextWhileWaiting(t *testing.T) {
	s := newSerializer()
	if err := s.lock(context.Background(), "conv-1"); err != nil {
		t.Fatalf("lock: %v", err)
	}

	wctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.lock(wctx, "conv-1") }()
	waitUntil(t, "a queued waiter", func() bool { return waiterCount(s, "conv-1") == 1 })

	cancel()
	err := <-result
	if !IsCode(err, CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
	// The cancelled waiter must leave the queue, so the holder's unlock does
	// not hand the key to a channel nobody is reading.
	waitUntil(t, "the cancelled waiter to leave the queue", func() bool {
		return waiterCount(s, "conv-1") == 0
	})

	s.unlock("conv-1")
	// The key must be free for a fresh caller.
	if err := s.lock(context.Background(), "conv-1"); err != nil {
		t.Fatalf("lock after cancellation: %v", err)
	}
	s.unlock("conv-1")
}

func TestSerializerRejectsAlreadyCancelledContext(t *testing.T) {
	s := newSerializer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.lock(ctx, "conv-1"); !IsCode(err, CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
}

func TestSerializerFIFOOrder(t *testing.T) {
	s := newSerializer()
	const key = "conv-1"
	if err := s.lock(context.Background(), key); err != nil {
		t.Fatalf("lock: %v", err)
	}

	order := make(chan int, 2)
	// Each goroutine announces itself, then queues. The waitUntil between
	// launches guarantees waiter[0] is first and waiter[1] is second, so the
	// expected FIFO order is known rather than racy.
	for i := 0; i < 2; i++ {
		go func(i int) {
			if err := s.lock(context.Background(), key); err != nil {
				t.Errorf("lock %d: %v", i, err)
				return
			}
			order <- i
			s.unlock(key)
		}(i)
		waitUntil(t, "the next waiter to queue", func() bool { return waiterCount(s, key) == i+1 })
	}

	s.unlock(key)
	if a, b := <-order, <-order; a != 0 || b != 1 {
		t.Fatalf("acquisition order = [%d %d], want [0 1]", a, b)
	}
}

func TestSerializerUnlockUnknownKeyIsHarmless(t *testing.T) {
	s := newSerializer()
	s.unlock("nope") // must not panic
}

func TestWrapContextPreservesErrorsIs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := wrapContext(ctx.Err())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("wrapContext must keep context.Canceled on the chain")
	}
	if !IsCode(err, CodeCanceled) {
		t.Fatalf("code = %q, want CANCELED", CodeOf(err))
	}
}
