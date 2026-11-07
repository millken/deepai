package bot

import (
	"sync"
	"time"
)

// DefaultReplayEvents is how many recent events each bus keeps for clients
// that reconnect with a "since" cursor. A turn of a few hundred text deltas
// fits comfortably; older history is served from the session transcript.
const DefaultReplayEvents = 4096

// subscriberBuffer bounds how far a subscriber may fall behind before it is
// dropped. Publishing never blocks on a slow client: a dropped subscriber
// sees its channel close with Lagged set and is expected to resubscribe
// with since=<last seq it saw>.
const subscriberBuffer = 1024

// bus fans events out to subscribers and keeps a bounded replay buffer.
type bus struct {
	mu     sync.Mutex
	base   int64 // seq before the first event of this bus
	seq    int64
	ring   []Event
	start  int // index of the oldest event in ring once it has wrapped
	full   bool
	subs   map[*Subscription]struct{}
	closed bool
}

func newBus(replay int) *bus { return newBusAt(replay, 0) }

// newBusAt starts numbering after base, so cursors handed out by an earlier
// bus of the same session (base > every seq it issued) are recognisable as
// stale instead of aliasing this bus's events.
func newBusAt(replay int, base int64) *bus {
	if replay <= 0 {
		replay = DefaultReplayEvents
	}
	return &bus{base: base, seq: base, ring: make([]Event, 0, replay), subs: map[*Subscription]struct{}{}}
}

// Subscription is a live feed of a bus.
type Subscription struct {
	C <-chan Event

	c      chan Event
	b      *bus
	once   sync.Once
	lagged bool
}

// Lagged reports whether the subscription was dropped for falling behind.
// Only meaningful after C has been closed.
func (s *Subscription) Lagged() bool {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.lagged
}

// Close detaches the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.closeLocked(false)
}

func (s *Subscription) closeLocked(lagged bool) {
	s.once.Do(func() {
		s.lagged = lagged
		delete(s.b.subs, s)
		close(s.c)
	})
}

// publish stamps evt with the next sequence number and delivers it.
func (b *bus) publish(evt Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return evt
	}
	b.seq++
	evt.Seq = b.seq
	if evt.Time.IsZero() {
		evt.Time = time.Now().UTC()
	}
	if len(b.ring) < cap(b.ring) {
		b.ring = append(b.ring, evt)
	} else {
		b.ring[b.start] = evt
		b.start = (b.start + 1) % len(b.ring)
		b.full = true
	}
	for s := range b.subs {
		select {
		case s.c <- evt:
		default:
			s.closeLocked(true)
		}
	}
	return evt
}

// subscribe registers a subscriber and returns the buffered events with
// Seq > since. Registration and the backlog snapshot happen under one lock,
// so no event can fall between the backlog and the live feed. since < 0
// means "live only, no backlog"; since == 0 means "from the first event".
//
// gap is true whenever the backlog cannot be the exact continuation of the
// client's cursor: events after since were evicted, or since is
// ahead of this bus (the session was unloaded and reloaded, and a fresh
// bus restarted at seq 1). The client should reload the transcript and
// resubscribe from the session's current last_seq.
func (b *bus) subscribe(since int64) (sub *Subscription, backlog []Event, gap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := make(chan Event, subscriberBuffer)
	sub = &Subscription{C: c, c: c, b: b}
	if b.closed {
		close(c)
		return sub, nil, false
	}
	b.subs[sub] = struct{}{}
	if since < 0 {
		return sub, nil, false
	}
	ordered := b.orderedLocked()
	if since == 0 {
		since = b.base
	}
	switch {
	case since < 0, since == b.seq:
	case since > b.seq:
		gap = true
	case len(ordered) == 0 || since < ordered[0].Seq-1:
		gap = true
	}
	for _, e := range ordered {
		if e.Seq > since {
			backlog = append(backlog, e)
		}
	}
	return sub, backlog, gap
}

func (b *bus) orderedLocked() []Event {
	if !b.full {
		return b.ring
	}
	out := make([]Event, 0, len(b.ring))
	out = append(out, b.ring[b.start:]...)
	return append(out, b.ring[:b.start]...)
}

func (b *bus) lastSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// close ends every subscription; later publishes are dropped.
func (b *bus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		s.closeLocked(false)
	}
}
