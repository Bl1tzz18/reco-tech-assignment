// Package obs holds the observability plumbing: structured logging with
// per-context attributes and secret redaction, process metrics, and the
// HTTP endpoints that expose them (health, metrics, pprof).
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"
)

const redacted = "[REDACTED]"

// sensitiveKeys are log attribute keys whose values are never printed, as a
// safety net in case someone logs a secret by mistake.
var sensitiveKeys = []string{"token", "api_token", "authorization", "password", "secret"}

type ctxKey struct{}

// With returns a ctx that carries extra log attributes (e.g. job, run). Every
// log call that receives this ctx (InfoContext, WarnContext, ...) on a logger
// built by NewLogger includes them, so one run can be followed end to end
// across packages without passing loggers around.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	r := slog.NewRecord(time.Time{}, 0, "", 0)
	r.Add(args...)
	attrs := slices.Clip(prev) // never write into the parent's backing array
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return context.WithValue(ctx, ctxKey{}, attrs)
}

// NewLogger builds a logger. format is "text" or "json"; level is "debug",
// "info", "warn" or "error".
func NewLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl, ReplaceAttr: replaceAttr}
	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", format)
	}
	return slog.New(ctxHandler{h}), nil
}

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
	}
	return lvl, nil
}

// replaceAttr redacts sensitive keys and prints durations as "1.5s" instead
// of nanoseconds (the JSON handler's default), so dashboards stay readable.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if slices.Contains(sensitiveKeys, strings.ToLower(a.Key)) {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindDuration {
		return slog.String(a.Key, a.Value.Duration().String())
	}
	return a
}

// ctxHandler adds the attributes stored by With to every record.
type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok && len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{h.Handler.WithGroup(name)}
}
