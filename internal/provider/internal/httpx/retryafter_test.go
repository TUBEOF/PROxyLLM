package httpx

import (
	"net/http"
	"testing"
	"time"
)

// Retry-After is either delta-seconds or an HTTP-date (RFC 9110). Both
// forms become a positive duration; a past date or a non-positive delta is
// "no hint", never a negative cooldown.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "120", 2 * time.Minute},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"future date", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"past date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.raw, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
