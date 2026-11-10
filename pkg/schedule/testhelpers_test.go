package schedule

import (
	"sync"
	"time"
)

// fakeClock is an injectable Clock. Advance fires every waiter whose deadline
// has passed, so Scheduler tests never sleep on wall time.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t.UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.now.Add(d)
	if d <= 0 {
		ch <- at
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: at, ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []fakeWaiter
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			due = append(due, w)
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, w := range due {
		select {
		case w.ch <- w.at:
		default:
		}
	}
}

func eventually(t interface {
	Helper()
	Fatalf(string, ...any)
}, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within 3s")
}
