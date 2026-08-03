package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryFidelityClaimNamesASuiteThatExists.
//
// examples/README.md answers "how do I know this behaves like Fabric?" by
// pointing at the e2e suites that prove each claim. That makes it a map, and a
// map to somewhere that no longer exists is worse than no map — a reader
// follows it, finds nothing, and concludes the claim was never true.
//
// This is the tenth failure in docs/10-testing.md: prose asserting a contract
// with nothing to make it go red. The README is prose asserting many. So every
// path it links is required to exist, and a suite renamed or removed fails
// here rather than in a reader's browser.
func TestEveryFidelityClaimNamesASuiteThatExists(t *testing.T) {
	root := filepath.Join("..", "..")
	readme := filepath.Join(root, "examples", "README.md")
	b, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}

	// Markdown links of the form (../e2e/<name>/) and (../docs/<file>).
	link := regexp.MustCompile(`\]\(\.\./((?:e2e|docs)/[^)]+)\)`)
	matches := link.FindAllStringSubmatch(string(b), -1)

	// A check that found nothing would pass forever while the README rotted.
	if len(matches) < 15 {
		t.Fatalf("only %d ../e2e or ../docs link(s) found in examples/README.md; "+
			"the link shape has drifted and this test is checking nothing",
			len(matches))
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range matches {
		rel := strings.TrimSuffix(m[1], "/")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			missing = append(missing, rel)
		}
	}
	if len(missing) > 0 {
		t.Errorf("examples/README.md points at paths that do not exist:\n  %s",
			strings.Join(missing, "\n  "))
	}

	// The claims are only worth making if the suites RUN. A suite with no
	// run.py is not wired into anything, so citing it as proof is a promise
	// nobody keeps.
	var unrunnable []string
	for rel := range seen {
		if !strings.HasPrefix(rel, "e2e/") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, rel, "run.py")); err != nil {
			unrunnable = append(unrunnable, rel)
		}
	}
	if len(unrunnable) > 0 {
		t.Errorf("cited as proof but has no run.py, so nothing executes it:\n  %s",
			strings.Join(unrunnable, "\n  "))
	}
}
