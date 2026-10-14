package webui

// The console's token layer exists twice on purpose. design/colors_and_type.css
// is the source the design system documents and the examples load;
// src/styles/colors_and_type.css is the copy this package embeds and the SPA
// bundles. Two copies of a palette drift, and a drifted copy is exactly the
// failure the design system cannot afford: a rule the documentation states and
// the app does not render, or a value the app renders that no document knows.
//
// Keep them the same file. Edit design/colors_and_type.css and copy it over.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const (
	sourceStylesheet  = "../design/colors_and_type.css"
	runtimeStylesheet = "src/styles/colors_and_type.css"
)

func TestTokenLayerCopiesAreIdentical(t *testing.T) {
	source, err := os.ReadFile(sourceStylesheet)
	if err != nil {
		t.Fatalf("read the token source: %v", err)
	}
	runtime, err := os.ReadFile(runtimeStylesheet)
	if err != nil {
		t.Fatalf("read the runtime token copy: %v", err)
	}

	// A truncated or emptied file would satisfy equality against itself, so
	// pin the public surface first: the roles every rule in the app addresses.
	for _, role := range []string{"--bg:", "--surface:", "--fg:", "--muted:", "--border:", "--accent:", "--accent-dim:"} {
		if !strings.Contains(string(source), role) {
			t.Errorf("%s no longer declares %s — the public token API must stay complete", sourceStylesheet, role)
		}
	}

	if !bytes.Equal(source, runtime) {
		t.Errorf(`%s and %s have drifted apart.

The token source is %s; the runtime copy is generated from it. Update the source
and copy the file over:

    cp %s %s

then rebuild the SPA (npm run build) so the embedded assets match.`,
			sourceStylesheet, runtimeStylesheet,
			sourceStylesheet, sourceStylesheet, runtimeStylesheet)
	}
}
