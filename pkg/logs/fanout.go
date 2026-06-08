package logs

import (
	"context"
	"errors"
	"log/slog"
)

// FanoutHandler writes log records to multiple handlers simultaneously.
// All handlers are attempted; errors are aggregated via errors.Join.
type FanoutHandler struct {
	handlers []slog.Handler
}

// NewFanoutHandler creates a handler that fans out to all given handlers.
func NewFanoutHandler(handlers ...slog.Handler) *FanoutHandler {
	return &FanoutHandler{handlers: handlers}
}

func (h *FanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, r.Level) {
			if err := hh.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (h *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		hs[i] = hh.WithAttrs(attrs)
	}
	return &FanoutHandler{handlers: hs}
}

func (h *FanoutHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		hs[i] = hh.WithGroup(name)
	}
	return &FanoutHandler{handlers: hs}
}
