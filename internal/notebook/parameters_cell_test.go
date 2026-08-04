package notebook

import "testing"

// TestParametersCellIsExecutable: Fabric marks a parameterised notebook's
// defaults with `# PARAMETERS CELL`, and every pipeline-driven notebook keeps
// its P_* declarations there.
//
// Treating that line as ordinary text rather than a delimiter does not drop the
// marker, it drops the CELL: the declarations fold into the preceding markdown
// and never run, so the notebook dies later on `NameError: name 'P_...' is not
// defined` and the parser that ate them is the last place anyone looks.
func TestParametersCellIsExecutable(t *testing.T) {
	src := []byte(`# Fabric notebook source

# MARKDOWN ********************

# ### a heading that must not absorb the parameters

# PARAMETERS CELL ********************

P_RUN_ID = None
P_MODE = "N"

# CELL ********************

print(P_RUN_ID, P_MODE)
`)

	code := CodeCells(Parse(src))
	if len(code) != 2 {
		t.Fatalf("expected the parameters cell and the body cell; got %d: %+v", len(code), code)
	}
	if got := code[0].Source; got != "P_RUN_ID = None\nP_MODE = \"N\"" {
		t.Fatalf("parameters cell not parsed as its own code cell: %q", got)
	}
	if code[0].Kind != Code {
		t.Fatalf("a parameters cell must execute; kind = %s", code[0].Kind)
	}
	// And the markdown above it stays markdown, rather than swallowing them.
	for _, c := range Parse(src) {
		if c.Kind == Markdown && len(c.Source) > 0 && contains(c.Source, "P_RUN_ID") {
			t.Fatalf("markdown cell absorbed the parameters: %q", c.Source)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
