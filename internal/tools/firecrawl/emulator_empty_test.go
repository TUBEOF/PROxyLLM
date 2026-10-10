package firecrawl

// Empty results and soft warnings (2026-10-10).
//
// Spec: a web-tool fallback account that answers "nothing" must be
// distinguishable from one that failed — the account walk has to continue past
// an empty answer instead of stopping on it. Grounded in the two empty-result
// fixtures, re-read from docs.firecrawl.dev on 2026-10-10:
//
//   - search: data.web is an empty array, success stays true. No minimum length
//     is imposed on data.web, so an empty result is an answer.
//   - scrape: a page Firecrawl could not serve is reported INSIDE a 200
//     response, via metadata.statusCode (the target's own status) and
//     metadata.error. To a caller the transport succeeded and the page did not.
//
// Plus the decoder contract for `warning`: the reference types it as a nullable
// string, the fixture this repo captured earlier carried an empty array, and a
// soft signal must not be able to fail a whole answer.

import (
	"context"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestWarningText_AcceptsEveryShapeSeenOnTheWire(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"null", `null`, ""},
		{"documented string", `"partial source degradation"`, "partial source degradation"},
		{"legacy empty array", `[]`, ""},
		{"array of strings", `["sources unavailable", "extraction partial"]`, "sources unavailable; extraction partial"},
		{"empty string", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w WarningText
			if err := w.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", tc.in, err)
			}
			if string(w) != tc.want {
				t.Errorf("warning = %q, want %q", string(w), tc.want)
			}
		})
	}
}

// The regression this guards: with the field typed []string, a non-empty
// warning string failed json.Unmarshal for the WHOLE response — so a soft
// "partial sources" note turned a successful search into an emulator error and
// the request paid a needless account walk.
func TestEmulator_SearchWithAStringWarningStillAnswers(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"warning":"1 of 3 sources unavailable","data":{"web":[
		{"title":"Go","url":"https://go.dev/","description":"d"}]}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	items, err := emu.Search(context.Background(), "go", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v — a warning must not fail the answer", err)
	}
	if len(items) != 1 || items[0].URL != "https://go.dev/" {
		t.Errorf("items = %+v, want the one result despite the warning", items)
	}
}

func TestEmulator_SearchWithNoMatchesIsAnEmptyAnswer(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, golden(t, "firecrawl/search-response-empty.json"))
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	items, err := emu.Search(context.Background(), "nothing matches this", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v — an empty result set is an answer, not an error (the walk must continue)", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
}

func TestEmulator_FetchOfAnUnservablePageIsAnEmptyAnswer(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, golden(t, "firecrawl/scrape-response-empty.json"))
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	fc, err := emu.Fetch(context.Background(), "https://example.com/missing", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v — a page the account could not serve must not fail the call", err)
	}
	if fc == nil {
		t.Fatal("Fetch returned no payload — the caller distinguishes empty by the payload's content, not by nil")
	}
	if fc.Data != "" {
		t.Errorf("fetched data = %q, want empty (metadata.statusCode 404 reports the page, not the transport)", fc.Data)
	}
	if fc.URL != "https://example.com/missing" {
		t.Errorf("url = %q, want the requested url", fc.URL)
	}
	if fc.Source != "text" || fc.MediaType != "text/plain" {
		t.Errorf("payload shape = %s/%s, want the documented text/plain normalization", fc.Source, fc.MediaType)
	}
}

// A page that answers 200 with content is served normally — the status guard
// must not swallow a good page.
func TestEmulator_FetchOfAServedPageKeepsItsContent(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, golden(t, "firecrawl/scrape-response.json"))
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	fc, err := emu.Fetch(context.Background(), "https://go.dev/dl/", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.Data == "" {
		t.Fatal("a 200 page with markdown came back empty")
	}
	if fc.Title != "Downloads - The Go Programming Language" {
		t.Errorf("title = %q, want the scraped title", fc.Title)
	}
}
