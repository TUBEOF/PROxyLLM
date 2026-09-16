// Package observability provides structured JSON logging and the redaction
// layer. All logging in PROxyLLM flows through this package — no log call
// site may bypass the redactor.
package observability

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tubeof/proxyllm/internal/config"
)

// NewLogger builds the application logger. Level and format come from
// configuration; the handler is always wrapped by the redacting handler —
// there is no code path that constructs an unredacted application logger.
func NewLogger(cfg config.LogConfig) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("observability: unknown log level %q", cfg.Level)
	}

	var inner slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	if strings.ToLower(cfg.Format) == "text" {
		inner = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	}

	return slog.New(NewRedactingHandler(inner, NewRedactor())), nil
}
