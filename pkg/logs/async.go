package logs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// asyncItem pairs a record with the handler that should process it.
type asyncItem struct {
	record  slog.Record
	handler slog.Handler
}

// AsyncHandler wraps a slog.Handler with asynchronous buffered writing.
// Records are queued and written by a background goroutine.
// When the queue is full, records are silently dropped and counted.
type AsyncHandler struct {
	inner   slog.Handler
	ch      chan asyncItem
	done    chan struct{}
	dropped *atomic.Int64
	once    sync.Once
}

// NewAsyncHandler creates an AsyncHandler with the given queue size.
// The background goroutine starts immediately.
func NewAsyncHandler(inner slog.Handler, queueSize int) *AsyncHandler {
	h := &AsyncHandler{
		inner:   inner,
		ch:      make(chan asyncItem, queueSize),
		done:    make(chan struct{}),
		dropped: &atomic.Int64{},
	}
	go h.drain()
	return h
}

func (h *AsyncHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *AsyncHandler) Handle(_ context.Context, r slog.Record) error {
	cp := r.Clone()
	select {
	case h.ch <- asyncItem{record: cp, handler: h.inner}:
	default:
		h.dropped.Add(1)
	}
	return nil
}

func (h *AsyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &AsyncHandler{
		inner:   h.inner.WithAttrs(attrs),
		ch:      h.ch,
		done:    h.done,
		dropped: h.dropped,
	}
}

func (h *AsyncHandler) WithGroup(name string) slog.Handler {
	return &AsyncHandler{
		inner:   h.inner.WithGroup(name),
		ch:      h.ch,
		done:    h.done,
		dropped: h.dropped,
	}
}

// Close stops accepting new records, drains the queue, and waits for completion.
func (h *AsyncHandler) Close() error {
	h.once.Do(func() {
		close(h.ch)
	})
	<-h.done
	return nil
}

// Dropped returns the total number of records dropped due to a full queue.
func (h *AsyncHandler) Dropped() int64 {
	return h.dropped.Load()
}

func (h *AsyncHandler) drain() {
	defer close(h.done)
	for item := range h.ch {
		item.handler.Handle(context.Background(), item.record)
	}
}

// Ensure AsyncHandler satisfies io.Closer.
var _ io.Closer = (*AsyncHandler)(nil)
