package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryLineageEdgeStatesItsProducer.
//
// CreateLineageEdge defaults an empty producer to Copy. That backstop is
// deliberate — an edge with no producer would leave a consumer unable to tell
// evidence from a claim — but it is dangerous as a MECHANISM, because Copy
// asserts the emulator watched the bytes move. A caller that simply forgot
// would be claiming evidence it never had, and nothing would say so.
//
// So the rule is: every construction site names its producer, and the default
// only ever catches a bug. A new call site that forgets fails here rather than
// quietly publishing a false claim of evidence into the lineage graph — which
// is the thing this project's whole producer vocabulary exists to prevent.
func TestEveryLineageEdgeStatesItsProducer(t *testing.T) {
	// Find `&store.LineageEdge{` ... `}` literals and require a Producer key.
	// Deliberately source-level: the alternative is asserting on behaviour the
	// default has already hidden.
	lit := regexp.MustCompile(`(?s)&store\.LineageEdge\{.*?\n\s*\}`)

	roots := []string{".", "../server"}
	var offenders []string
	seen := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range lit.FindAllString(string(b), -1) {
				seen++
				if !strings.Contains(m, "Producer:") {
					offenders = append(offenders,
						filepath.Join(root, name)+": "+strings.SplitN(m, "\n", 2)[0])
				}
			}
		}
	}

	// A test that found nothing to check would pass forever while the rule
	// rotted — the literal shape could move and this would say "fine".
	if seen < 5 {
		t.Fatalf("only %d LineageEdge literal(s) found; the pattern has drifted "+
			"and this test is no longer checking anything", seen)
	}
	if len(offenders) > 0 {
		t.Errorf("lineage edge(s) rely on the Copy default instead of stating a "+
			"producer — Copy claims the emulator WATCHED the movement:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
