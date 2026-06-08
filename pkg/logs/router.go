package logs

import (
	"context"
	"errors"
	"log/slog"
)

// route matches log records whose level falls within [min, max].
type route struct {
	min, max slog.Level
	handler  slog.Handler
}

// RouterHandler routes log records to different handlers based on level.
// Routes are matched in order; the first matching route handles the record.
type RouterHandler struct {
	routes []route
}

// NewRouterHandler creates a router with the given level-based routes.
func NewRouterHandler(routes ...route) *RouterHandler {
	return &RouterHandler{routes: routes}
}

func (h *RouterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, r := range h.routes {
		if level >= r.min && level <= r.max {
			return r.handler.Enabled(ctx, level)
		}
	}
	return false
}

func (h *RouterHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, rt := range h.routes {
		if r.Level >= rt.min && r.Level <= rt.max {
			if err := rt.handler.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (h *RouterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	routes := make([]route, len(h.routes))
	for i, r := range h.routes {
		routes[i] = route{min: r.min, max: r.max, handler: r.handler.WithAttrs(attrs)}
	}
	return &RouterHandler{routes: routes}
}

func (h *RouterHandler) WithGroup(name string) slog.Handler {
	routes := make([]route, len(h.routes))
	for i, r := range h.routes {
		routes[i] = route{min: r.min, max: r.max, handler: r.handler.WithGroup(name)}
	}
	return &RouterHandler{routes: routes}
}
