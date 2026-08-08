package api

import "testing"

// headerSelector parses the `headers…` form of a REST connector's pagination
// selector. It is small, pure, and entirely syntax — which is exactly the kind
// of function where an off-by-one is invisible in review and shows up as a
// connector reading the wrong header at runtime.
//
// The bracket forms carry their own trap: the length guards exist so `headers.`
// and `headers[”]` are rejected rather than resolving to the empty header
// name, which would match nothing and look like an absent header instead of a
// malformed selector.
func TestHeaderSelector(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"headers.Link", "Link", true},
		{"headers.x-next-page", "x-next-page", true},
		{"headers['Link']", "Link", true},
		{`headers["Link"]`, "Link", true},
		{"headers['x-next-page']", "x-next-page", true},

		// Malformed: a name is required after the accessor.
		{"headers.", "", false},
		{"headers['']", "", false},
		{`headers[""]`, "", false},

		// Not a header selector at all.
		{"headers", "", false},
		{"headers['Link\"]", "", false}, // mismatched quotes
		{"headers[Link]", "", false},    // unquoted
		{"headersLink", "", false},      // no accessor
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := headerSelector(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("headerSelector(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
