package memory

import (
	"testing"
	"time"

	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/storetest"
)

// fixedTime is the injected clock, so timestamps are exact in assertions.
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is a hand-cranked time source implementing domain.Clock.
type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

// TestContract runs the shared store conformance suite against the in-memory
// adapter. The suite is the contract; if this passes and anansistore passes,
// the two are interchangeable to the kernel.
func TestContract(t *testing.T) {
	storetest.Run(t, func(t testing.TB) store.Store {
		return New(Options{Clock: clock{t: fixedTime}})
	})
}
