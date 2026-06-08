package logs

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAsyncHandler_RecordsWritten(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 256)

	logger := slog.New(h)
	logger.Info("hello", "key", "val")

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("expected 'hello' in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "key=val") {
		t.Errorf("expected 'key=val' in output, got: %s", buf.String())
	}
}

func TestAsyncHandler_DropOnFullQueue(t *testing.T) {
	// Use a slow handler (blocking) and tiny queue to force drops.
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 2) // tiny queue

	logger := slog.New(h)
	for i := 0; i < 100; i++ {
		logger.Info("msg", "i", i)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if h.Dropped() == 0 {
		t.Error("expected some records to be dropped with queue size 2 and 100 writes")
	}
}

func TestAsyncHandler_CloseDrainsAll(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 256)

	logger := slog.New(h)
	const n = 50
	for i := 0; i < n; i++ {
		logger.Info("drain-test", "i", i)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	count := strings.Count(buf.String(), "drain-test")
	if count != n {
		t.Errorf("expected %d records after drain, got %d", n, count)
	}
}

func TestAsyncHandler_CloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 16)

	logger := slog.New(h)
	logger.Info("test")

	if err := h.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	// Second close should not panic or block.
	if err := h.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

func TestAsyncHandler_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 256)

	logger := slog.New(h)
	var wg sync.WaitGroup
	const goroutines = 50
	const writesPer = 20
	var totalWrites atomic.Int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < writesPer; i++ {
				logger.Info("concurrent", "g", g, "i", i)
				totalWrites.Add(1)
			}
		}(g)
	}
	wg.Wait()

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	written := totalWrites.Load()
	dropped := h.Dropped()
	count := strings.Count(buf.String(), "concurrent")
	if int64(count)+dropped != written {
		t.Errorf("written+dropped=%d+%d != total=%d, found=%d", count, dropped, written, count)
	}
}

func TestAsyncHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewAsyncHandler(inner, 16)

	logger := slog.New(h).With("component", "test")
	logger.Info("hello")

	h.Close()

	if !strings.Contains(buf.String(), "component=test") {
		t.Errorf("expected component=test, got: %s", buf.String())
	}
}
