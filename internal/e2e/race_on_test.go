//go:build race

package e2e

// Enabled reports whether the race detector is active in this test binary
// (the build-tag file pair: this variant is compiled with -race, which
// sets the race build tag).
const raceEnabled = true
