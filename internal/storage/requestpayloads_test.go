package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Spec: the Logs page can show what a request carried and what came back
// (2026-10-10). Payloads live in their own table keyed by the row they belong
// to, so the list query stays cheap; they are size-capped so one long agentic
// session cannot put a multi-megabyte row on disk per request; and they are
// pruned on a window, which is what keeps the cap from being a per-request
// bound on an unbounded table.

func TestRequestPayload_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	id := seedRequestLog(t, s, 1760000000)

	want := RequestPayload{
		OwnerKind:      PayloadOwnerRequest,
		OwnerID:        id,
		TS:             1760000000,
		ClientRequest:  `{"messages":[{"role":"user","content":"hi"}]}`,
		ClientResponse: `{"content":[{"type":"text","text":"hello"}]}`,
	}
	if err := s.PutRequestPayload(ctx, want); err != nil {
		t.Fatalf("PutRequestPayload: %v", err)
	}

	got, found, err := s.GetRequestPayload(ctx, PayloadOwnerRequest, id)
	if err != nil {
		t.Fatalf("GetRequestPayload: %v", err)
	}
	if !found {
		t.Fatal("payload not found after write")
	}
	if got.ClientRequest != want.ClientRequest || got.ClientResponse != want.ClientResponse {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Bytes != int64(len(want.ClientRequest)+len(want.ClientResponse)) {
		t.Errorf("Bytes = %d, want the stored size", got.Bytes)
	}
	if got.Truncated {
		t.Error("a small payload was marked truncated")
	}
}

func TestRequestPayload_AbsentIsNotFound(t *testing.T) {
	s := openTokenStore(t)
	got, found, err := s.GetRequestPayload(context.Background(), PayloadOwnerRequest, 4242)
	if err != nil {
		t.Fatalf("GetRequestPayload: %v", err)
	}
	if found {
		t.Errorf("found a payload that was never written: %+v", got)
	}
}

// Two rows of different kinds can share an id — a request row and a child row
// are numbered independently — so the key must be the pair.
func TestRequestPayload_KindIsPartOfTheKey(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutRequestPayload(ctx, RequestPayload{
		OwnerKind: PayloadOwnerRequest, OwnerID: 7, ClientRequest: "req",
	}); err != nil {
		t.Fatalf("put request payload: %v", err)
	}
	if err := s.PutRequestPayload(ctx, RequestPayload{
		OwnerKind: PayloadOwnerChild, OwnerID: 7, ClientRequest: "child",
	}); err != nil {
		t.Fatalf("put child payload: %v", err)
	}

	req, _, err := s.GetRequestPayload(ctx, PayloadOwnerRequest, 7)
	if err != nil {
		t.Fatalf("get request payload: %v", err)
	}
	child, _, err := s.GetRequestPayload(ctx, PayloadOwnerChild, 7)
	if err != nil {
		t.Fatalf("get child payload: %v", err)
	}
	if req.ClientRequest != "req" || child.ClientRequest != "child" {
		t.Errorf("the owner kind did not separate the rows: req=%q child=%q", req.ClientRequest, child.ClientRequest)
	}
}

// An over-long payload is stored as a marked prefix, and a reader can tell that
// it was cut — misreading a truncated conversation as the whole thing would be
// worse than not storing it.
func TestRequestPayload_OverlongIsCappedAndMarked(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	id := seedRequestLog(t, s, 1760000000)

	long := strings.Repeat("x", MaxPayloadBytes*2)
	if err := s.PutRequestPayload(ctx, RequestPayload{
		OwnerKind: PayloadOwnerRequest, OwnerID: id, ClientRequest: long,
	}); err != nil {
		t.Fatalf("PutRequestPayload: %v", err)
	}

	got, _, err := s.GetRequestPayload(ctx, PayloadOwnerRequest, id)
	if err != nil {
		t.Fatalf("GetRequestPayload: %v", err)
	}
	if !got.Truncated {
		t.Error("an over-long payload was not marked truncated")
	}
	if len(got.ClientRequest) > MaxPayloadBytes+256 {
		t.Errorf("stored %d bytes, want it bounded near the cap", len(got.ClientRequest))
	}
	// The marker has to survive as JSON: the drawer parses it.
	var envelope struct {
		Truncated    bool   `json:"_truncated"`
		OriginalSize int    `json:"_originalSize"`
		Preview      string `json:"_preview"`
	}
	if err := json.Unmarshal([]byte(got.ClientRequest), &envelope); err != nil {
		t.Fatalf("the truncation envelope is not valid JSON: %v", err)
	}
	if !envelope.Truncated || envelope.OriginalSize != len(long) {
		t.Errorf("envelope = %+v, want the original size %d", envelope, len(long))
	}
	if len(envelope.Preview) == 0 {
		t.Error("the envelope carries no preview")
	}
}

// An empty capture writes nothing: there is no conversation to show, and an
// empty row would only add work for the pruner.
func TestRequestPayload_EmptyIsNotStored(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutRequestPayload(ctx, RequestPayload{
		OwnerKind: PayloadOwnerRequest, OwnerID: 11,
	}); err != nil {
		t.Fatalf("PutRequestPayload: %v", err)
	}
	if _, found, err := s.GetRequestPayload(ctx, PayloadOwnerRequest, 11); err != nil || found {
		t.Errorf("an empty payload was stored (found=%v err=%v)", found, err)
	}
}

// Pruning drops only the captured conversation. The metadata row is the record
// that the request happened and must outlive it.
func TestRequestPayload_PruneKeepsTheLogRow(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	old := seedRequestLog(t, s, 1000)
	fresh := seedRequestLog(t, s, 2000)
	for id, ts := range map[int64]int64{old: 1000, fresh: 2000} {
		if err := s.PutRequestPayload(ctx, RequestPayload{
			OwnerKind: PayloadOwnerRequest, OwnerID: id, TS: ts, ClientRequest: "x",
		}); err != nil {
			t.Fatalf("PutRequestPayload: %v", err)
		}
	}

	deleted, err := s.PruneRequestPayloads(ctx, 1500)
	if err != nil {
		t.Fatalf("PruneRequestPayloads: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d rows, want 1", deleted)
	}
	if _, found, _ := s.GetRequestPayload(ctx, PayloadOwnerRequest, old); found {
		t.Error("the pruned payload survived")
	}
	if _, found, _ := s.GetRequestPayload(ctx, PayloadOwnerRequest, fresh); !found {
		t.Error("the fresh payload was pruned")
	}
	// The request row itself is untouched.
	rows, err := s.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == old {
			found = true
		}
	}
	if !found {
		t.Error("pruning removed the request log row")
	}
}

func TestRequestPayload_RejectsUnknownOwnerKind(t *testing.T) {
	s := openTokenStore(t)
	err := s.PutRequestPayload(context.Background(), RequestPayload{
		OwnerKind: "sessions", OwnerID: 1, ClientRequest: "x",
	})
	if err == nil {
		t.Fatal("an unknown owner kind must be refused, not written under a surprise key")
	}
}
