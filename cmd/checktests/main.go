// Command checktests guards the test suite against erosion.
//
// A test that is deleted, weakened or skipped still reports green, so a suite
// can rot without a single failure ever appearing. This compares the working
// tree against a base revision and refuses the changes that quietly reduce what
// is asserted:
//
//   - a deleted test file
//   - fewer assertions in a modified test file
//   - a newly added skip
//   - a tautology
//
// It also reports, without failing, assertions that re-encode a production
// condition locally: those stay green when the real condition drifts, which is
// the failure mode a review is least likely to catch by eye. Reporting them as a
// warning keeps the signal without turning style into a build break.
//
//	go run ./cmd/checktests            # against origin/main, or main
//	go run ./cmd/checktests -base HEAD~1
//
// A deliberate removal (a provider type deleted by an ADR, with its tests)
// is declared in allow.txt next to this file, one `path | reason` per line.
// A declared path is printed as an allowed removal, never silently skipped;
// new skips and tautologies still fail.
//
// Exit status: 0 clean (warnings may be printed), 1 findings, 2 the check could
// not run — a missing base revision is not a pass.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// assertionPattern counts the ways a Go test states a fact, and the ways a
// Playwright spec does. Deliberately call-shaped: a bare word would count prose.
var (
	goAssertion    = regexp.MustCompile(`\bt\.(Error|Errorf|Fatal|Fatalf|Fail|FailNow)\b`)
	tsAssertion    = regexp.MustCompile(`\bexpect\s*\(`)
	goSkip         = regexp.MustCompile(`\bt\.Skip(f|Now)?\s*\(`)
	tsSkip         = regexp.MustCompile(`\b(test|describe|it)\.(skip|todo)\b|\.(skip|only)\s*\(`)
	tautology      = regexp.MustCompile(`expect\s*\(\s*true\s*\)\s*\.toBe\s*\(\s*true\s*\)|assert\.True\s*\(\s*true\s*\)|if\s+1\s*!=\s*1`)
	isTestFile     = regexp.MustCompile(`_test\.go$|/[^/]+\.spec\.ts$`)
	reEncodedProd  = regexp.MustCompile(`(?i)#\s*(re-?implements|mirrors|same as)\b`)
	reEncodedProd2 = regexp.MustCompile(`(?i)//\s*the (same|identical) (condition|predicate|check)`)
)

type finding struct {
	file   string
	detail string
}

func main() {
	base := flag.String("base", "", "revision to compare against (default: origin/main, else main)")
	allowPath := flag.String("allow", "cmd/checktests/allow.txt", "declared intentional removals (path | reason)")
	flag.Parse()

	if err := run(*base, *allowPath); err != nil {
		fmt.Fprintln(os.Stderr, "checktests:", err)
		os.Exit(2)
	}
}

func run(baseFlag, allowPath string) error {
	base, err := resolveBase(baseFlag)
	if err != nil {
		return err
	}
	allow, err := loadAllow(allowPath)
	if err != nil {
		return err
	}

	deleted, err := gitLines("diff", "--name-only", "--diff-filter=D", base)
	if err != nil {
		return err
	}
	modified, err := gitLines("diff", "--name-only", "--diff-filter=M", base)
	if err != nil {
		return err
	}

	var findings []finding
	var warnings []finding

	allowed := filterDeletions(deleted, allow, &findings)

	for _, f := range modified {
		if !isTestFile.MatchString(f) {
			continue
		}
		before, err := gitShow(base, f)
		if err != nil {
			return err
		}
		after, err := os.ReadFile(f) //nolint:gosec // f is a repo-relative path taken from git
		if err != nil {
			return err
		}
		found, warn := compareFile(f, string(before), string(after))
		if reason, ok := allow[f]; ok {
			if len(found) > 0 {
				allowed = append(allowed, finding{f, reason})
			}
			found = nil
		}
		findings = append(findings, found...)
		warnings = append(warnings, warn...)
	}

	added, err := gitLines("diff", "-U0", base)
	if err != nil {
		return err
	}
	findings = append(findings, newSkips(added)...)

	for _, a := range allowed {
		fmt.Printf("allowed removal: %s: %s\n", a.file, a.detail)
	}
	for _, w := range warnings {
		fmt.Printf("warning: %s: %s\n", w.file, w.detail)
	}
	if len(findings) == 0 {
		fmt.Printf("checktests: no test erosion against %s\n", base)
		return nil
	}
	for _, f := range findings {
		fmt.Printf("erosion: %s: %s\n", f.file, f.detail)
	}
	return fmt.Errorf("%d finding(s) against %s", len(findings), base)
}

// loadAllow reads the declared intentional removals: one repo-relative path
// and a non-empty reason, separated by "|". A path without a reason is an
// error — an unexplained removal is exactly what this tool exists to stop.
func loadAllow(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path) //nolint:gosec // a path the operator passes on the command line
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		file, reason, ok := strings.Cut(line, "|")
		file, reason = strings.TrimSpace(file), strings.TrimSpace(reason)
		if !ok || file == "" || reason == "" {
			return nil, fmt.Errorf("%s:%d: want \"path | reason\"", path, n+1)
		}
		if out[file] != "" {
			return nil, fmt.Errorf("%s:%d: %s is declared twice", path, n+1, file)
		}
		out[file] = reason
	}
	return out, nil
}

// filterDeletions separates declared removals from erosion. Deleted non-test
// files are not this tool's business.
func filterDeletions(deleted []string, allow map[string]string, findings *[]finding) []finding {
	var allowed []finding
	for _, f := range deleted {
		if !isTestFile.MatchString(f) {
			continue
		}
		if reason, ok := allow[f]; ok {
			allowed = append(allowed, finding{f, reason})
			continue
		}
		*findings = append(*findings, finding{f, "test file deleted"})
	}
	return allowed
}

// compareFile counts what a test file asserts before and after the change.
func compareFile(file, before, after string) (findings, warnings []finding) {
	counter := goAssertion
	skip := goSkip
	if strings.HasSuffix(file, ".spec.ts") {
		counter, skip = tsAssertion, tsSkip
	}
	// Prose is not an assertion. A comment that mentions t.Errorf, or a test
	// name that reads like one, must not count — otherwise rewriting a comment
	// looks like weakening a test.
	was, now := len(counter.FindAllString(code(before), -1)), len(counter.FindAllString(code(after), -1))
	if now < was {
		findings = append(findings, finding{file,
			fmt.Sprintf("assertions went from %d to %d", was, now)})
	}
	if len(skip.FindAllString(code(after), -1)) > len(skip.FindAllString(code(before), -1)) {
		findings = append(findings, finding{file, "a skip was added"})
	}
	if tautology.MatchString(code(after)) && !tautology.MatchString(code(before)) {
		findings = append(findings, finding{file, "a tautology was added"})
	}
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.HasPrefix(strings.TrimSpace(line), "*") {
			continue
		}
		if (reEncodedProd.MatchString(line) || reEncodedProd2.MatchString(line)) &&
			!strings.Contains(before, line) {
			warnings = append(warnings, finding{file,
				"assertion may re-encode a production condition: " + strings.TrimSpace(line)})
		}
	}
	return findings, warnings
}

// newSkips looks at the added lines of the whole diff for a skip that did not
// exist before — a file-level count can hide one skip replaced by another.
func newSkips(diffLines []string) []finding {
	var out []finding
	file := ""
	for _, line := range diffLines {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			file = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+") && isTestFile.MatchString(file):
			body := strings.TrimPrefix(line, "+")
			if goSkip.MatchString(body) || tsSkip.MatchString(body) {
				out = append(out, finding{file, "added: " + strings.TrimSpace(body)})
			}
		}
	}
	return out
}

// code strips line comments so only what the compiler sees is counted. A
// block comment's opening line goes too; the middle of one still counts, which
// errs toward not reporting rather than toward crying wolf.
func code(contents string) string {
	lines := strings.Split(contents, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

func resolveBase(flagValue string) (string, error) {
	if flagValue != "" {
		if !gitOK("rev-parse", "--verify", flagValue) {
			return "", fmt.Errorf("base revision %q does not exist", flagValue)
		}
		return flagValue, nil
	}
	for _, candidate := range []string{"origin/main", "main"} {
		if gitOK("rev-parse", "--verify", candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no base revision found (tried origin/main, main); pass -base")
}

func gitOK(args ...string) bool {
	return exec.Command("git", args...).Run() == nil //nolint:gosec // git, with arguments this program builds
}

func gitLines(args ...string) ([]string, error) {
	out, err := exec.Command("git", args...).Output() //nolint:gosec // git, with arguments this program builds
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := sc.Text(); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

func gitShow(rev, path string) ([]byte, error) {
	out, err := exec.Command("git", "show", rev+":"+path).Output() //nolint:gosec // git, with a revision and a repo-relative path from the diff
	if err != nil {
		// A file added by this change has no base version: zero assertions is
		// the honest reading, not an error.
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 128 {
			return nil, nil
		}
		return nil, fmt.Errorf("git show %s:%s: %w", rev, path, err)
	}
	return out, nil
}
