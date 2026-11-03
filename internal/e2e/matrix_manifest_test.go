package e2e

// The conformance matrix manifest (testdata/matrix.json) is the single source
// of truth for the matrix: every client surface × upstream wire × feature ×
// mode is one cell, green or pending. The manifest test below validates the
// manifest itself — the cross product must be complete, ids unique, every
// pending cell carries a reason — and the runners execute every green cell.
// A missing combination is a failure here, never a silent skip.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// matrixCell is one matrix entry: a pairing under test, or an extra scenario
// (faults, web emulation, meta controls) outside the pair cross product.
type matrixCell struct {
	ID       string `json:"id"`
	Client   string `json:"client,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Feature  string `json:"feature,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Test     string `json:"test,omitempty"`
	Status   string `json:"status"`
	Note     string `json:"note,omitempty"`
	// SchemaNote documents, for a green pair cell, why its client-facing
	// response is not yet validated against the official wire schema (a
	// measured, nameable emission gap; the fix loop owns the parity). The
	// runner prints the note whenever the cell runs, so the gap stays
	// visible on every matrix run — never a silent skip.
	SchemaNote string `json:"schema_note,omitempty"`
}

// matrixManifest mirrors testdata/matrix.json.
type matrixManifest struct {
	Schema    int          `json:"schema"`
	Clients   []string     `json:"clients"`
	Upstreams []string     `json:"upstreams"`
	Features  []string     `json:"features"`
	Modes     []string     `json:"modes"`
	Cells     []matrixCell `json:"cells"`
	Extras    []matrixCell `json:"extras"`
}

// loadMatrix reads the manifest; it is pure data validation and runs without
// MockServer, so it belongs in the ordinary test suite too.
func loadMatrix(t *testing.T) matrixManifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/matrix.json")
	if err != nil {
		t.Fatalf("read testdata/matrix.json: %v", err)
	}
	var m matrixManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse testdata/matrix.json: %v", err)
	}
	return m
}

// TestMatrix_ManifestIsComplete validates the manifest itself: the pair cross
// product must be fully enumerated, ids unique, every pending cell justified,
// and every non-pending pair cell claimable by the runners. This is the
// completeness gate — a matrix that quietly drops a combination fails here.
func TestMatrix_ManifestIsComplete(t *testing.T) {
	m := loadMatrix(t)
	if m.Schema != 2 {
		t.Fatalf("manifest schema = %d, want 2", m.Schema)
	}
	if len(m.Clients) == 0 || len(m.Upstreams) == 0 || len(m.Features) == 0 || len(m.Modes) == 0 {
		t.Fatal("manifest axes must not be empty")
	}

	seen := map[string]matrixCell{}
	for _, c := range m.Cells {
		if c.ID == "" {
			t.Fatal("a cell carries no id")
		}
		if prev, dup := seen[c.ID]; dup {
			t.Fatalf("duplicate cell id %q (%+v vs %+v)", c.ID, prev, c)
		}
		seen[c.ID] = c
		switch c.Status {
		case "green":
			if c.Note != "" {
				t.Errorf("green cell %q carries a note: %q", c.ID, c.Note)
			}
			if c.SchemaNote != "" {
				// A documented schema gap is only meaningful on a pair cell
				// the runners execute, and must state itself precisely.
				if !strings.HasPrefix(c.SchemaNote, "schema: not yet strict — ") {
					t.Errorf("schema_note on %q must describe the not-yet-strict state (got %q)", c.ID, c.SchemaNote)
				}
				if c.Client == "" || c.Feature == "" || c.Mode == "" {
					t.Errorf("schema_note on %q rides a pair cell, not an extra", c.ID)
				}
			}
		case "pending":
			if c.Note == "" {
				t.Errorf("pending cell %q carries no reason", c.ID)
			}
			if c.SchemaNote != "" {
				t.Errorf("pending cell %q carries a schema_note; only green pair cells document gaps", c.ID)
			}
		default:
			t.Errorf("cell %q status = %q, want green|pending", c.ID, c.Status)
		}
	}

	// The cross product must be fully enumerated. A missing combination means
	// the matrix is incomplete by construction, not merely unimplemented.
	for _, client := range m.Clients {
		for _, upstream := range m.Upstreams {
			for _, feature := range m.Features {
				for _, mode := range m.Modes {
					id := cellID(client, upstream, feature, mode)
					if _, ok := seen[id]; !ok {
						t.Errorf("missing cell %q — the cross product must be complete", id)
					}
				}
			}
		}
	}

	for _, c := range m.Extras {
		if c.ID == "" || (c.Status == "pending" && c.Note == "") {
			t.Errorf("invalid extra cell: %+v", c)
		}
	}
}

// cellID derives the canonical cell id.
func cellID(client, upstream, feature, mode string) string {
	return fmt.Sprintf("%s__%s__%s__%s", client, upstream, feature, mode)
}

// greenPairCells returns the cells the runners execute for one feature.
func greenPairCells(t *testing.T, feature string) []matrixCell {
	t.Helper()
	m := loadMatrix(t)
	var out []matrixCell
	for _, c := range m.Cells {
		if c.Feature == feature && c.Status == "green" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no green cells for feature %q", feature)
	}
	return out
}
