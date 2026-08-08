package api

import (
	"net/http"
	"testing"
)

// The pagination selectors decide whether a REST connector takes another page
// or stops. They were the least-covered code in the connector — `headerSelector`
// at 0%, `resolveValue` at 50%, `isEmptyNode` at 33% — and every uncovered arm
// is either a selector spelling Fabric accepts or one of its documented STOP
// conditions. An unreached stop condition is the dangerous half: a selector that
// wrongly reports "keep going" pages forever against a real API.

// headerSelector accepts the spellings Fabric's expressions use. The false
// cases matter more than the true ones: each is a malformed selector that must
// not resolve to the empty header name, which `hdr.Get("")` would answer with
// "" and a caller could read as a legitimate empty value.
func TestHeaderSelectorSpellings(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want string
		ok   bool
	}{
		{"headers.Link", "Link", true},
		{"headers['X-Next-Page']", "X-Next-Page", true},
		{`headers["X-Next-Page"]`, "X-Next-Page", true},
		// Malformed: a name is required, and an empty one is not a name.
		{"headers.", "", false},
		{"headers['']", "", false},
		{`headers[""]`, "", false},
		// Bracket forms must be closed, and in the same quote style.
		{"headers['X-Next", "", false},
		{`headers["X-Next']`, "", false},
		// Neither spelling at all.
		{"headers", "", false},
		{"headers-Link", "", false},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			got, ok := headerSelector(tc.expr)
			if got != tc.want || ok != tc.ok {
				t.Errorf("headerSelector(%q) = (%q, %v), want (%q, %v)",
					tc.expr, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// resolveValue evaluates one pagination value against a response. `ok=false` is
// a STOP, so each false case below is a page-loop terminator: a missing path, an
// explicit null, an absent header, a malformed selector, an empty literal.
func TestResolveValueAndItsStopConditions(t *testing.T) {
	doc := map[string]any{
		"next":   "cursor-2",
		"count":  float64(17),
		"null":   nil,
		"nested": map[string]any{"token": "abc"},
		"empty":  "",
	}
	hdr := http.Header{}
	hdr.Set("X-Next", "page-3")
	hdr.Set("X-Blank", "")

	for _, tc := range []struct {
		name, expr, want string
		ok               bool
	}{
		{"jsonpath string", "$.next", "cursor-2", true},
		{"jsonpath number renders without a float tail", "$.count", "17", true},
		{"jsonpath nested", "$.nested.token", "abc", true},
		{"jsonpath missing STOPS", "$.absent", "", false},
		{"explicit null STOPS", "$.null", "", false},
		{"header present", "headers.X-Next", "page-3", true},
		{"header case-insensitive prefix", "Headers.X-Next", "page-3", true},
		{"header absent STOPS", "headers.X-Missing", "", false},
		{"header present but empty STOPS", "headers.X-Blank", "", false},
		{"malformed selector STOPS", "headers.", "", false},
		{"literal passes through", "42", "42", true},
		{"empty literal STOPS", "", "", false},
		{"whitespace-only literal STOPS", "   ", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveValue(tc.expr, doc, hdr)
			if got != tc.want || ok != tc.ok {
				t.Errorf("resolveValue(%q) = (%q, %v), want (%q, %v)",
					tc.expr, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// isEmptyNode is the other stop condition: a page whose collection came back
// empty. It must recognise emptiness in each JSON shape a collection can take,
// and must NOT call a zero or a false empty — those are values, and treating
// them as end-of-collection would truncate a real result set.
func TestIsEmptyNodeAcrossJSONShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		node any
		want bool
	}{
		{"nil", nil, true},
		{"empty array", []any{}, true},
		{"empty object", map[string]any{}, true},
		{"empty string", "", true},
		{"populated array", []any{1}, false},
		{"populated object", map[string]any{"a": 1}, false},
		{"non-empty string", "x", false},
		// Values, not emptiness — the cases a laxer check would truncate on.
		{"zero is a value", float64(0), false},
		{"false is a value", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyNode(tc.node); got != tc.want {
				t.Errorf("isEmptyNode(%#v) = %v, want %v", tc.node, got, tc.want)
			}
		})
	}
}
