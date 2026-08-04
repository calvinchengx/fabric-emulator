package notebook

import "testing"

// Fabric applies a caller's overrides by adding a cell BENEATH the designated
// parameters cell, so a driver has to know which cell that is. It is not always
// the first: a notebook may import, or %run a setup notebook, before declaring
// its defaults. Aiming at the first code cell in that shape writes the caller's
// values and then lets the parameters cell assign its placeholders over them,
// so the run proceeds on the defaults the caller explicitly replaced.
func TestParametersCellIdentifiedWhenNotFirst(t *testing.T) {
	src := []byte(`# Fabric notebook source

# MARKDOWN ********************

# ### setup

# CELL ********************

import json

# PARAMETERS CELL ********************

P_LAKEHOUSE = "placeholder"

# CELL ********************

print(P_LAKEHOUSE)
`)
	code := CodeCells(Parse(src))
	if len(code) != 3 {
		t.Fatalf("want 3 code cells, got %d: %+v", len(code), code)
	}
	if code[0].Parameters {
		t.Errorf("cell 0 (%q) is a plain CELL, not the parameters cell", code[0].Source)
	}
	if !code[1].Parameters {
		t.Errorf("cell 1 (%q) is the PARAMETERS cell but was not marked", code[1].Source)
	}
	if code[2].Parameters {
		t.Errorf("cell 2 (%q) is a plain CELL, not the parameters cell", code[2].Source)
	}
}

// The common shape: the parameters cell IS first. The flag must still be set,
// and no plain cell may be mistaken for it.
func TestParametersCellIdentifiedWhenFirst(t *testing.T) {
	src := []byte(`# Fabric notebook source

# PARAMETERS CELL ********************

P_X = 1

# CELL ********************

print(P_X)
`)
	code := CodeCells(Parse(src))
	if len(code) != 2 {
		t.Fatalf("want 2 code cells, got %d: %+v", len(code), code)
	}
	if !code[0].Parameters {
		t.Errorf("the PARAMETERS cell was not marked: %q", code[0].Source)
	}
	if code[1].Parameters {
		t.Errorf("a plain CELL was marked as parameters: %q", code[1].Source)
	}
}

// A notebook with no parameters cell designates nothing, and markdown is never
// a parameters cell even though it sits before one.
func TestNoParametersCell(t *testing.T) {
	src := []byte(`# Fabric notebook source

# MARKDOWN ********************

# just docs

# CELL ********************

print("hi")
`)
	for _, c := range Parse(src) {
		if c.Parameters {
			t.Errorf("nothing is designated here, but %s cell %q was marked", c.Kind, c.Source)
		}
	}
}
