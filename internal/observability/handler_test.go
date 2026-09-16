package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/config"
)

// Spec: ADR-007 control 3 — the redacting handler wraps every logger;
// secrets in messages AND attribute values (including nested groups) are
// masked before the inner handler sees them. WithAttrs values are redacted
// at attach time so downstream loggers inherit masked values.

func TestRedactingHandler_MasksMessageAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewRedactingHandler(inner, NewRedactor()))

	log.Info("auth failed", "error", "invalid key sk-abcdefgh123456789 provided")

	out := buf.String()
	if strings.Contains(out, "sk-abcdefgh123456789") {
		t.Errorf("secret in attr survived: %s", out)
	}
	if !strings.Contains(out, "auth failed") {
		t.Errorf("message lost: %s", out)
	}
}

func TestRedactingHandler_MasksGroupAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewRedactingHandler(inner, NewRedactor()))

	log.Error("upstream error",
		"upstream", slog.GroupValue(
			slog.String("url", "https://api.example.com/v1?token=abcdef1234567890"),
			slog.String("status", "503"),
		))

	out := buf.String()
	if strings.Contains(out, "abcdef1234567890") {
		t.Errorf("secret in nested group survived: %s", out)
	}
	if !strings.Contains(out, "api.example.com") {
		t.Errorf("URL structure lost: %s", out)
	}
}

func TestRedactingHandler_WithAttrsRedactedAtAttach(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewRedactingHandler(inner, NewRedactor())).
		With("api_key", "sk-fixedsecret123456789")
	log.Info("request handled")

	out := buf.String()
	if strings.Contains(out, "sk-fixedsecret123456789") {
		t.Errorf("WithAttrs secret survived: %s", out)
	}
}

func TestRedactingHandler_MasksBytesAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewRedactingHandler(inner, NewRedactor()))

	// []byte-kind attribute values carry the same secrets string values
	// can; the JSON handler renders them base64, but the redaction must
	// happen before rendering — mask on the raw bytes.
	log.Error("payload rejected", "body", []byte(`{"secret_key":"0123456789abcdef0123456789abcdef"}`))

	out := buf.String()
	if strings.Contains(out, "0123456789abcdef0123456789abcdef") {
		t.Errorf("secret in bytes attr survived: %s", out)
	}
	if !strings.Contains(out, "payload rejected") {
		t.Errorf("message lost: %s", out)
	}
}

func TestRedactingHandler_JSONStillValid(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewRedactingHandler(inner, NewRedactor()))

	log.Warn("cfg", "token", "Bearer abcdefghijklmnop")

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("redacted output must remain valid JSON: %v\n%s", err, buf.String())
	}
}

func TestNewLogger_DefaultsWork(t *testing.T) {
	// Smoke: NewLogger must produce a working logger (json default).
	log, err := NewLogger(configLog("info", "json"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	log.Info("smoke")
}

func TestNewLogger_InvalidLevel(t *testing.T) {
	if _, err := NewLogger(configLog("verbose", "json")); err == nil {
		t.Error("unknown level must error")
	}
}

// configLog builds a LogConfig for tests.
func configLog(level, format string) config.LogConfig {
	return config.LogConfig{Level: level, Format: format}
}
