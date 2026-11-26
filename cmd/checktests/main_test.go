package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allow.txt")
	body := "# comment\n\ninternal/provider/x/x_test.go | provider removed by ADR-099\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAllow(path)
	if err != nil {
		t.Fatalf("loadAllow: %v", err)
	}
	if got["internal/provider/x/x_test.go"] != "provider removed by ADR-099" {
		t.Fatalf("allow map = %v", got)
	}

	missing, err := loadAllow(filepath.Join(dir, "absent.txt"))
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing file = %v, %v; want empty map, nil", missing, err)
	}
}

func TestLoadAllowRejectsUnexplainedRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allow.txt")
	if err := os.WriteFile(path, []byte("internal/provider/x/x_test.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAllow(path); err == nil {
		t.Fatal("a path without a reason must be rejected")
	}
}

func TestFilterDeletions(t *testing.T) {
	allow := map[string]string{"internal/provider/x/x_test.go": "ADR-099"}
	var findings []finding
	allowed := filterDeletions([]string{
		"internal/provider/x/x_test.go",
		"internal/provider/y/y_test.go",
		"internal/provider/x/x.go",
	}, allow, &findings)

	if len(allowed) != 1 || allowed[0].file != "internal/provider/x/x_test.go" {
		t.Fatalf("allowed = %v, want the declared test file", allowed)
	}
	if len(findings) != 1 || findings[0].file != "internal/provider/y/y_test.go" {
		t.Fatalf("findings = %v, want only the undeclared test file", findings)
	}
}

func TestCompareFileStillFailsOnNewSkip(t *testing.T) {
	before := "func TestX(t *testing.T) {\n\tt.Errorf(\"x\")\n}\n"
	after := "func TestX(t *testing.T) {\n\tt.Skip(\"later\")\n\tt.Errorf(\"x\")\n}\n"
	findings, _ := compareFile("internal/x/x_test.go", before, after)
	if len(findings) != 1 || findings[0].detail != "a skip was added" {
		t.Fatalf("findings = %v, want the added skip", findings)
	}
}
