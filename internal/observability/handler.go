package observability

import (
	"context"
	"log/slog"
)

// redactingHandler wraps any slog.Handler and redacts every attribute value
// (and the message) through the Redactor before delegation. This is the
// structural enforcement of ADR-007 control 3: log call sites cannot bypass
// redaction because the only loggers the application constructs wrap their
// handlers here.
type redactingHandler struct {
	inner    slog.Handler
	redactor Redactor
}

// NewRedactingHandler wraps inner so every emitted value passes the
// redactor first.
func NewRedactingHandler(inner slog.Handler, r Redactor) slog.Handler {
	return &redactingHandler{inner: inner, redactor: r}
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	masked := slog.NewRecord(rec.Time, rec.Level, h.redactor.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		masked.AddAttrs(redactAttr(h.redactor, a))
		return true
	})
	return h.inner.Handle(ctx, masked)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(h.redactor, a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(redacted), redactor: h.redactor}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), redactor: h.redactor}
}

// redactAttr masks string- and []byte-kind values; groups are recursed.
func redactAttr(r Redactor, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		grp := a.Value.Group()
		out := make([]slog.Attr, len(grp))
		for i, ga := range grp {
			out[i] = redactAttr(r, ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if a.Value.Kind() == slog.KindString {
		return slog.Attr{Key: a.Key, Value: slog.StringValue(r.Redact(a.Value.String()))}
	}
	if a.Value.Kind() == slog.KindAny {
		// slog has no dedicated bytes kind (slog.Any wraps []byte as
		// KindAny); bytes carry the same payloads strings do, so mask on
		// the raw bytes before the inner handler renders them.
		if bs, ok := a.Value.Any().([]byte); ok {
			return slog.Attr{Key: a.Key, Value: slog.StringValue(r.Redact(string(bs)))}
		}
	}
	return a
}
