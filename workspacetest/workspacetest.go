// Package workspacetest holds the doubles every workspace test needs:
// a fake clock, a deterministic ID generator, a scripted provider, a faulty
// store for failure injection, an event log that tests wait on rather than
// sleep on, and the harness that wires a real workspace around them.
//
// Four rules keep the core testable (Workspace specification, section 16):
// no test needs a network, a real clock or a real database; time and IDs are
// injected; every port has a contract suite; and tests wait on events, never
// on sleeps.
package workspacetest

import (
	"sync"
	"time"
)

// FakeClock is a hand-cranked clock. Nothing in the core calls time.Now, so
// a test advances time explicitly and the code under test sees only what the
// test allowed.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts a fake clock at the given instant.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// SeqIDs mints predictable IDs, so a test can assert on a specific ID rather
// than only on its shape.
type SeqIDs struct {
	mu     sync.Mutex
	prefix string
	n      int
}

// NewSeqIDs returns a generator whose IDs are prefix-1, prefix-2, ...
func NewSeqIDs(prefix string) *SeqIDs {
	return &SeqIDs{prefix: prefix}
}

// NewID returns the next ID in the sequence.
func (g *SeqIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return g.prefix + "-" + itoa(g.n)
}

// itoa is a tiny decimal formatter, so the package stays stdlib-only.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
