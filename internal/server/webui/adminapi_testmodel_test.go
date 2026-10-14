package webui

// Model test endpoint contract tests (Models page):
//   - Unwired TestModel (no master key / harness) answers the uniform 404,
//     identical to the sync-run degrade contract.
//   - A wired test returns the result wire shape {ok, latency_ms, reply,
//     error}; unknown models are ok:false with a plain-language error.
//   - The endpoint is session-gated like every mutating admin route.

import (
	"context"
	"net/http"
	"testing"
)

func TestModelTest_UnwiredIs404(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodPost, "/api/admin/models/test/m1", "", s)
	assertStatus(t, w, http.StatusNotFound, "unwired model test")
}

func TestModelTest_WiredReturnsResult(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	e.api.testModel = func(_ context.Context, _ string) (TestModelResult, error) {
		return TestModelResult{OK: true, LatencyMS: 12, Reply: "OK"}, nil
	}

	w := e.authedCall(t, http.MethodPost, "/api/admin/models/test/m1", "", s)
	assertStatus(t, w, http.StatusOK, "wired model test")
	var resp TestModelResult
	decodeJSON(t, w, &resp)
	if !resp.OK || resp.LatencyMS != 12 || resp.Reply != "OK" {
		t.Errorf("result = %+v, want ok + latency + reply", resp)
	}
}

func TestModelTest_WiredFailureCarriesError(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	e.api.testModel = func(_ context.Context, _ string) (TestModelResult, error) {
		return TestModelResult{OK: false, Error: "upstream rejected the test request (HTTP 400): nope"}, nil
	}

	w := e.authedCall(t, http.MethodPost, "/api/admin/models/test/m1", "", s)
	assertStatus(t, w, http.StatusOK, "failed round trips are still 200 — ok:false carries the cause")
	var resp TestModelResult
	decodeJSON(t, w, &resp)
	if resp.OK || resp.Error == "" {
		t.Errorf("result = %+v, want ok:false + error", resp)
	}
}

func TestModelTest_RequireAdmin(t *testing.T) {
	e := newAdminEnv(t)
	_ = e.bootstrap(t, "admin")

	w := e.call(t, http.MethodPost, "/api/admin/models/test/m1", "", "")
	assertStatus(t, w, http.StatusUnauthorized, "no session")
}
