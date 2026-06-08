package logs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type failingHandler struct {
	called bool
}

func (h *failingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *failingHandler) Handle(_ context.Context, _ slog.Record) error {
	h.called = true
	return errors.New("fail")
}
func (h *failingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *failingHandler) WithGroup(_ string) slog.Handler      { return h }

func TestFanoutHandler_WritesToAll(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	fanout := NewFanoutHandler(
		slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug}),
		slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	logger := slog.New(fanout)
	logger.Info("fanout-msg", "key", "val")

	if !strings.Contains(buf1.String(), "fanout-msg") {
		t.Errorf("handler1 should contain fanout-msg, got: %s", buf1.String())
	}
	if !strings.Contains(buf2.String(), "fanout-msg") {
		t.Errorf("handler2 should contain fanout-msg, got: %s", buf2.String())
	}
}

func TestFanoutHandler_ContinuesOnFailure(t *testing.T) {
	var buf bytes.Buffer
	fail := &failingHandler{}
	fanout := NewFanoutHandler(
		fail,
		slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	logger := slog.New(fanout)
	err := logger.Handler().Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0))

	if err == nil {
		t.Error("expected aggregated error from failing handler")
	}
	if !fail.called {
		t.Error("failing handler should have been called")
	}
	if !strings.Contains(buf.String(), "test") {
		t.Errorf("second handler should still receive the record, got: %s", buf.String())
	}
}

func TestFanoutHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	fanout := NewFanoutHandler(
		slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	logger := slog.New(fanout).With("component", "x")
	logger.Info("msg")

	if !strings.Contains(buf.String(), "component=x") {
		t.Errorf("expected component=x, got: %s", buf.String())
	}
}
