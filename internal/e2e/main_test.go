package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/testutil/mockserver"
)

// TestMain owns the optional testcontainers MockServer (ADR-027d): with
// MOCKSERVER_CONTAINER=1 and no MOCKSERVER_URL, one container serves the whole
// package run. Without the variable nothing changes — CI's service container
// (MOCKSERVER_URL), a local JAR, or the documented skip.
func TestMain(m *testing.M) {
	if os.Getenv("MOCKSERVER_CONTAINER") == "1" && os.Getenv("MOCKSERVER_URL") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		url, stop, err := mockserver.StartContainer(ctx)
		cancel()
		if err != nil {
			panic(err)
		}
		_ = os.Setenv("MOCKSERVER_URL", url)
		code := m.Run()
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}
