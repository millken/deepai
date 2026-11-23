package proxy

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// Shutdown racing an in-flight emitEvents (issue #51): emitEvents checked
// shuttingDown without a lock and then sent on logCh, while Shutdown closed
// logCh right after httpServer.Shutdown — a sender that passed the check
// just before close() panicked with "send on closed channel". The mutex
// makes send-vs-close mutually exclusive; this stress runs the racy
// interleaving across many fresh proxies (Shutdown closes logCh exactly
// once per proxy, so each iteration needs a new one).
func TestShutdown_NoSendOnClosedLogCh(t *testing.T) {
	for i := 0; i < 300; i++ {
		p, err := NewProxy(slog.Default(), Config{Addr: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					p.emitEvents([]LogEvent{{Timestamp: time.Now().UTC(), Type: EventDone, ID: "r"}})
				}
			}()
		}

		// Shutdown: flag → httpServer.Shutdown (never listened: instant) →
		// close(logCh). The send-vs-close window is nanoseconds wide; the
		// four spinners on the other Ps are what hits it.
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		close(stop)
		wg.Wait()

		// A post-shutdown emit must be dropped, not panic.
		p.emitEvents([]LogEvent{{Timestamp: time.Now().UTC(), Type: EventDone, ID: "late"}})
	}
}
