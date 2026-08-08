package api

// Where a pagination VALUE comes from, as opposed to how the loop steps.
//
// restpagination_test.go drives whole paging sequences and asserts the requests
// the server saw. That covers the loop well and the value layer barely: a
// cursor read out of a response HEADER — half of what the file's own doc
// comment promises ("a JSONPath into the body, or `Headers.x`") — had no test
// at all, and neither did the scalar rendering every rule depends on.
//
// These are direct unit tests because the functions are pure, and because the
// interesting cases are refusals: a value that cannot be resolved must report
// false so the loop STOPS. A helper that returned ("", true) instead would page
// forever against a live endpoint, and no happy-path sequence would show it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeaderSelectorSpellings(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want string
		ok   bool
	}{
		{"headers.x-next-page", "x-next-page", true},
		{"headers['X-Next']", "X-Next", true},
		{`headers["X-Next"]`, "X-Next", true},
		// A selector naming nothing is not a header called "": resolving it
		// would read an absent header and stop the loop for the wrong reason.
		{"headers.", "", false},
		{"headers['']", "", false},
		{"headers", "", false},
		// Unbalanced quoting is a malformed rule, not a header named `'X`.
		{"headers['X", "", false},
	} {
		got, ok := headerSelector(tc.expr)
		if got != tc.want || ok != tc.ok {
			t.Errorf("headerSelector(%q) = (%q, %v), want (%q, %v)", tc.expr, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolveValueReadsBodyHeaderAndLiteral(t *testing.T) {
	doc := map[string]any{
		"paging": map[string]any{"next": "https://example.test/p2", "done": nil},
		"count":  float64(42),
	}
	hdr := http.Header{}
	hdr.Set("X-Next-Cursor", "abc")

	for _, tc := range []struct {
		name string
		expr string
		want string
		ok   bool
	}{
		{"jsonpath", "$.paging.next", "https://example.test/p2", true},
		{"jsonpath number", "$.count", "42", true},
		// Null is Fabric's documented stop condition — not the string "null".
		{"jsonpath null", "$.paging.done", "", false},
		{"jsonpath missing", "$.paging.absent", "", false},
		{"header", "Headers.X-Next-Cursor", "abc", true},
		{"header bracket", "Headers['X-Next-Cursor']", "abc", true},
		// An absent header stops the loop; it does not send an empty cursor.
		{"header missing", "Headers.X-Absent", "", false},
		{"header malformed", "Headers.", "", false},
		// A constant is unambiguous: it can be neither a path nor a header.
		{"literal", "static-token", "static-token", true},
		{"empty", "", "", false},
	} {
		got, ok := resolveValue(tc.expr, doc, hdr)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: resolveValue(%q) = (%q, %v), want (%q, %v)",
				tc.name, tc.expr, got, ok, tc.want, tc.ok)
		}
	}
}

// A cursor lands in a URL, so 1000 must not render as 1000.0 — JSON has one
// number type and every offset arrives here as a float64.
func TestScalarStringRendersJSONScalars(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
		ok   bool
	}{
		{"string", "abc", "abc", true},
		{"empty string", "", "", false},
		{"integral float", float64(1000), "1000", true},
		{"negative integral", float64(-5), "-5", true},
		{"fractional", 1.5, "1.5", true},
		{"bool", true, "true", true},
		// A cursor cannot be an object or an array: rendering one would put
		// `map[...]` in a URL rather than stopping.
		{"object", map[string]any{"a": 1}, "", false},
		{"array", []any{1}, "", false},
		{"nil", nil, "", false},
	} {
		got, ok := scalarString(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: scalarString(%#v) = (%q, %v), want (%q, %v)",
				tc.name, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// `EndCondition: Empty` is how an endpoint that returns `{"items":[]}` forever
// terminates, so what counts as empty is load-bearing.
func TestIsEmptyNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, true},
		{"empty array", []any{}, true},
		{"empty object", map[string]any{}, true},
		{"empty string", "", true},
		{"populated array", []any{1}, false},
		{"populated object", map[string]any{"a": 1}, false},
		{"non-empty string", "x", false},
		// Zero is a value, not an absence: an offset of 0 must keep paging.
		{"zero", float64(0), false},
		{"false", false, false},
	} {
		if got := isEmptyNode(tc.in); got != tc.want {
			t.Errorf("%s: isEmptyNode(%#v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// Rule keys are the author's spelling of where a value goes. Every accepted
// spelling has to resolve to the same place, and anything else has to be
// refused by name — a rule silently ignored is a paging loop that quietly reads
// page one and reports success, which is the failure R1 existed to prevent.
func TestSplitRuleKeyAcceptsEverySpellingAndRefusesTheRest(t *testing.T) {
	for _, tc := range []struct {
		key       string
		where     string
		sel       string
		wantError bool
	}{
		{key: "AbsoluteUrl", where: "AbsoluteUrl"},
		{key: "QueryParameters.offset", where: "QueryParameters", sel: "offset"},
		{key: "QueryParameters['offset']", where: "QueryParameters", sel: "offset"},
		{key: `QueryParameters["offset"]`, where: "QueryParameters", sel: "offset"},
		{key: "Headers.X-Token", where: "Headers", sel: "X-Token"},
		{key: "Headers['X-Token']", where: "Headers", sel: "X-Token"},
		// Not a family we implement.
		{key: "Body.next", wantError: true},
		// Right family, unparseable selector: the brackets never close.
		{key: "QueryParameters['offset", wantError: true},
		// Case matters — Fabric's keys are capitalised, and guessing at
		// `queryparameters` would accept a rule the product rejects.
		{key: "queryParameters.offset", wantError: true},
		{key: "", wantError: true},
	} {
		where, sel, err := splitRuleKey("act", tc.key)
		if tc.wantError {
			if err == nil {
				t.Errorf("splitRuleKey(%q) = (%q, %q, nil), want an error", tc.key, where, sel)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitRuleKey(%q): %v", tc.key, err)
			continue
		}
		if where != tc.where || sel != tc.sel {
			t.Errorf("splitRuleKey(%q) = (%q, %q), want (%q, %q)", tc.key, where, sel, tc.where, tc.sel)
		}
	}
}

// RANGE:start:end:step. A malformed range must be refused at parse time rather
// than producing a loop that steps by zero and never terminates.
func TestParseRangeRefusesMalformedRanges(t *testing.T) {
	ok := []struct {
		val    string
		start  float64
		step   float64
		hasEnd bool
		end    float64
	}{
		{val: "RANGE:0::1000", start: 0, step: 1000},
		{val: "RANGE:0:5000:1000", start: 0, step: 1000, hasEnd: true, end: 5000},
		{val: "range:10:20:5", start: 10, step: 5, hasEnd: true, end: 20},
		// Negative steps page backwards, which is unusual but well-defined.
		{val: "RANGE:100:0:-10", start: 100, step: -10, hasEnd: true, end: 0},
	}
	for _, tc := range ok {
		r, err := parseRange("act", "QueryParameters.{offset}", tc.val)
		if err != nil {
			t.Errorf("parseRange(%q): %v", tc.val, err)
			continue
		}
		if r.cur != tc.start || r.step != tc.step || r.hasEnd != tc.hasEnd || (tc.hasEnd && r.end != tc.end) {
			t.Errorf("parseRange(%q) = %+v, want start %v step %v hasEnd %v end %v",
				tc.val, r, tc.start, tc.step, tc.hasEnd, tc.end)
		}
	}
	for _, bad := range []string{
		"0::1000",           // not a RANGE at all
		"RANGE:0:1000",      // two fields, not three
		"RANGE:0::1000:2",   // four
		"RANGE:x::1000",     // start is not a number
		"RANGE:0::y",        // step is not a number
		"RANGE:0::0",        // a zero step never advances
		"RANGE:0:notanum:1", // end is not a number
	} {
		if _, err := parseRange("act", "QueryParameters.{offset}", bad); err == nil {
			t.Errorf("parseRange(%q) was accepted, want a refusal", bad)
		}
	}
}

func TestResolveRelativeHandlesAbsoluteRelativeAndMalformed(t *testing.T) {
	for _, tc := range []struct{ base, next, want string }{
		{"http://h/a/b", "https://other/x", "https://other/x"},
		{"http://h/a/b", "/page2", "http://h/page2"},
		{"http://h/a/b", "page2", "http://h/a/page2"},
		{"http://h/a/b?x=1", "?x=2", "http://h/a/b?x=2"},
		// An unparseable base or next is returned as-is rather than mangled:
		// the request will fail loudly at dial time, which is the honest place.
		{"http://h/\x7f", "next", "next"},
		{"http://h/a", "http://\x7f", "http://\x7f"},
	} {
		if got := resolveRelative(tc.base, tc.next); got != tc.want {
			t.Errorf("resolveRelative(%q, %q) = %q, want %q", tc.base, tc.next, got, tc.want)
		}
	}
}

// The half of the documented cursor sources that had no test: the next page's
// token arrives in a response header rather than the body.
func TestPaginationFollowsACursorFromAResponseHeader(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("X-Next-Cursor", "a")
			fmt.Fprint(w, `{"entries":[{"id":1}]}`)
		case "a":
			w.Header().Set("X-Next-Cursor", "b")
			fmt.Fprint(w, `{"entries":[{"id":2}]}`)
		default:
			// No header on the last page: absence is the stop.
			fmt.Fprint(w, `{"entries":[{"id":3}]}`)
		}
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{"QueryParameters.cursor": "Headers.X-Next-Cursor"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("want 3 rows across 3 pages, got %d", len(tbl.Rows))
	}
	want := []string{"GET /", "GET /?cursor=a", "GET /?cursor=b"}
	if got := rec.requests(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
}

// A rule can also SET a header on the next request from one on the response —
// the shape APIs use when the continuation token is not URL-safe.
func TestPaginationCarriesACursorIntoTheNextRequestHeader(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		if r.Header.Get("X-Continuation") == "" {
			w.Header().Set("X-Token", "tok-1")
			fmt.Fprint(w, `{"entries":[{"id":1}]}`)
			return
		}
		fmt.Fprint(w, `{"entries":[{"id":2}]}`)
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{"Headers.X-Continuation": "Headers.X-Token"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(tbl.Rows))
	}
	if len(rec.hdrs) != 2 {
		t.Fatalf("saw %d requests, want 2", len(rec.hdrs))
	}
	if got := rec.hdrs[1].Get("X-Continuation"); got != "tok-1" {
		t.Fatalf("second request X-Continuation = %q, want the token from page one", got)
	}
}

// An EndCondition can name a response header, not just a body path.
func TestPaginationEndConditionOnAResponseHeader(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		page := r.URL.Query().Get("cursor")
		if page == "a" {
			// Still a cursor to follow, but the end condition fires first —
			// which is the only way to tell the two rules apart.
			w.Header().Set("X-Complete", "true")
			w.Header().Set("X-Next-Cursor", "b")
			fmt.Fprint(w, `{"entries":[{"id":2}]}`)
			return
		}
		w.Header().Set("X-Next-Cursor", "a")
		fmt.Fprint(w, `{"entries":[{"id":1}]}`)
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{
			"QueryParameters.cursor":          "Headers.X-Next-Cursor",
			"EndCondition:$.unused":           "Exist",
			"EndCondition:Headers.X-Complete": "Const:true",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the header end condition did not stop the loop", len(tbl.Rows))
	}
	if got := rec.requests(); len(got) != 2 {
		t.Fatalf("request sequence = %v, want 2 pages", got)
	}
}
