package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Pagination tests assert the SEQUENCE OF REQUESTS the server saw, not just the
// final row count. A loop that fetched the same page twice, or stopped one page
// early, can produce a plausible row count either way — the request log is the
// only thing that distinguishes "paged correctly" from "happened to add up".

// recorder is a paging server that records every request it serves.
type recorder struct {
	mu     sync.Mutex
	seen   []string // "METHOD path?query"
	hdrs   []http.Header
	bodies []string
}

func (r *recorder) log(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := req.URL.Path
	if req.URL.RawQuery != "" {
		q += "?" + req.URL.RawQuery
	}
	r.seen = append(r.seen, req.Method+" "+q)
	r.hdrs = append(r.hdrs, req.Header.Clone())
	buf := make([]byte, req.ContentLength)
	if req.ContentLength > 0 {
		_, _ = req.Body.Read(buf)
	}
	r.bodies = append(r.bodies, string(buf))
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestPaginationFollowsAnAbsoluteUrlCursor(t *testing.T) {
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		switch r.URL.Query().Get("cursor") {
		case "":
			fmt.Fprintf(w, `{"entries":[{"id":1}],"paging":{"next":%q}}`, srv.URL+"/?cursor=a")
		case "a":
			fmt.Fprintf(w, `{"entries":[{"id":2}],"paging":{"next":%q}}`, srv.URL+"/?cursor=b")
		default:
			// Last page: the cursor is null, which is Fabric's documented stop.
			fmt.Fprint(w, `{"entries":[{"id":3}],"paging":{"next":null}}`)
		}
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{"AbsoluteUrl": "$.paging.next"},
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

func TestPaginationAcceptsARelativeNextLink(t *testing.T) {
	// Fabric documents AbsoluteUrl as "either absolute URL or relative URL".
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		if r.URL.Path == "/page2" {
			fmt.Fprint(w, `{"entries":[{"id":2}]}`)
			return
		}
		fmt.Fprint(w, `{"entries":[{"id":1}],"next":"/page2"}`)
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/start",
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %d", len(tbl.Rows))
	}
	if got := rec.requests(); got[1] != "GET /page2" {
		t.Fatalf("relative link not resolved against the base: %v", got)
	}
}

func TestPaginationStepsAQueryParameterRange(t *testing.T) {
	// Microsoft's own first example, and BMC Helix's shape: an offset stepped
	// until the records run out.
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		switch r.URL.Query().Get("offset") {
		case "0", "2":
			fmt.Fprint(w, `{"entries":[{"id":1},{"id":2}]}`)
		default:
			fmt.Fprint(w, `{"entries":[]}`) // exhausted
		}
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/incident?limit=2&offset={offset}",
		"paginationRules": map[string]any{
			"QueryParameters.{offset}": "RANGE:0::2",
			"EndCondition:$.entries":   "Empty",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 4 {
		t.Fatalf("want 4 rows over two full pages, got %d", len(tbl.Rows))
	}
	want := []string{
		"GET /incident?limit=2&offset=0",
		"GET /incident?limit=2&offset=2",
		"GET /incident?limit=2&offset=4",
	}
	if got := rec.requests(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("request sequence = %v, want %v", got, want)
	}
}

func TestPaginationStopsAtABoundedRangeEnd(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		fmt.Fprint(w, `{"entries":[{"id":1}]}`) // never runs out on its own
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/t?page={p}",
		"paginationRules": map[string]any{"AbsoluteUrl.{p}": "RANGE:1:3:1"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /t?page=1", "GET /t?page=2", "GET /t?page=3"}
	if got := rec.requests(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("a bounded range must stop at its end: %v", got)
	}
}

func TestPaginationStepsAHeaderRange(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		fmt.Fprint(w, `{"entries":[{"id":1}]}`)
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL,
		"additionalHeaders": map[string]any{"X-Page": "{p}"},
		"paginationRules":   map[string]any{"Headers.{p}": "RANGE:0:20:10"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var pages []string
	for _, h := range rec.hdrs {
		pages = append(pages, h.Get("X-Page"))
	}
	if strings.Join(pages, ",") != "0,10,20" {
		t.Fatalf("header range = %v, want 0,10,20", pages)
	}
}

func TestPaginationEndConditions(t *testing.T) {
	// `first` is per-case on purpose: `Exist` fires on a key that is PRESENT
	// whatever its value, so a first page carrying `"complete":false` would end
	// the loop immediately — correct behaviour, wrong fixture. That mistake is
	// worth encoding rather than papering over with one shared body.
	for _, tc := range []struct {
		name, rule, cond string
		first, last      string
		wantPages        int
	}{
		{"Empty", "EndCondition:$.entries", "Empty",
			`{"entries":[{"id":1}],"next":%q}`, `{"entries":[]}`, 2},
		{"NonExist", "EndCondition:$.next", "NonExist",
			`{"entries":[{"id":1}],"next":%q}`, `{"entries":[{"id":9}]}`, 2},
		{"Exist", "EndCondition:$.complete", "Exist",
			`{"entries":[{"id":1}],"next":%q}`, `{"entries":[{"id":9}],"complete":true}`, 2},
		{"Const", "EndCondition:$.complete", "Const:true",
			`{"entries":[{"id":1}],"next":%q,"complete":false}`, `{"entries":[{"id":9}],"complete":true}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.log(r)
				if r.URL.Query().Get("p") == "1" {
					fmt.Fprint(w, tc.last)
					return
				}
				fmt.Fprintf(w, tc.first, srv.URL+"/?p=1")
			}))
			defer srv.Close()

			if _, _, err := restSrc(t, &API{}, map[string]any{
				"type": "RestSource", "url": srv.URL + "/",
				"paginationRules": map[string]any{"AbsoluteUrl": "$.next", tc.rule: tc.cond},
			}, nil); err != nil {
				t.Fatal(err)
			}
			if n := len(rec.requests()); n != tc.wantPages {
				t.Fatalf("%s: %d pages, want %d (%v)", tc.name, n, tc.wantPages, rec.requests())
			}
		})
	}
}

func TestPaginationMaxRequestNumberCaps(t *testing.T) {
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		fmt.Fprintf(w, `{"entries":[{"id":1}],"next":%q}`, srv.URL+"/more")
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next", "MaxRequestNumber": "2"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.requests()); n != 2 {
		t.Fatalf("MaxRequestNumber 2 served %d pages", n)
	}
}

func TestPaginationStopsOn204(t *testing.T) {
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		if r.URL.Path == "/next" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprintf(w, `{"entries":[{"id":1}],"next":%q}`, srv.URL+"/next")
	}))
	defer srv.Close()

	tbl, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/",
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 || len(rec.requests()) != 2 {
		t.Fatalf("204 must end the loop cleanly: rows=%d pages=%d", len(tbl.Rows), len(rec.requests()))
	}
}

func TestPaginationRefusesAnEndlessSelfReferentialNext(t *testing.T) {
	// Microsoft documents this exact case: `next` in the last response equals the
	// last request URL, so the loop never ends. Without a ceiling this test hangs
	// forever, which is the point of having one.
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		fmt.Fprintf(w, `{"entries":[{"id":1}],"next":%q}`, srv.URL+"/same")
	}))
	defer srv.Close()

	_, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/same",
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next"},
	}, nil)
	if err == nil {
		t.Fatal("an endless next must be refused, not looped forever")
	}
	if !strings.Contains(err.Error(), "MaxRequestNumber") {
		t.Fatalf("the refusal should name the knob that prevents it: %v", err)
	}
	if n := len(rec.requests()); n != restMaxPages {
		t.Fatalf("served %d pages, want the %d ceiling", n, restMaxPages)
	}
}

func TestPaginationFollowsRFC5988ByDefaultAndOnlyThen(t *testing.T) {
	// Fabric turns SupportRFC5988 on when NO rule is declared. That is surprising
	// enough that both halves are asserted: it fires with no rules, and it does
	// NOT fire once the author has declared their own.
	serve := func(rec *recorder) *httptest.Server {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.log(r)
			if r.URL.Path == "/p2" {
				fmt.Fprint(w, `{"entries":[{"id":2}]}`)
				return
			}
			w.Header().Set("Link", `<`+srv.URL+`/p2>; rel="next"`)
			fmt.Fprint(w, `{"entries":[{"id":1}]}`)
		}))
		return srv
	}

	t.Run("no rules: followed", func(t *testing.T) {
		rec := &recorder{}
		srv := serve(rec)
		defer srv.Close()
		tbl, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL + "/p1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tbl.Rows) != 2 {
			t.Fatalf("Link rel=next should be followed by default: rows=%d", len(tbl.Rows))
		}
	})

	t.Run("rules declared: not followed", func(t *testing.T) {
		rec := &recorder{}
		srv := serve(rec)
		defer srv.Close()
		// An explicit rule set whose cursor is absent: the loop must stop rather
		// than fall back to the Link header the author did not ask for.
		tbl, _, err := restSrc(t, &API{}, map[string]any{
			"type": "RestSource", "url": srv.URL + "/p1",
			"paginationRules": map[string]any{"AbsoluteUrl": "$.nothing"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tbl.Rows) != 1 {
			t.Fatalf("declared rules must not fall back to RFC 5988: rows=%d", len(tbl.Rows))
		}
	})

	// These two reach the fallback branch itself. The subtest above does not: its
	// `AbsoluteUrl` rule resolves to nothing and returns before the Link header is
	// ever considered, so it pins the OUTCOME without exercising the guard —
	// mutation testing caught exactly that, by flipping the guard and staying green.
	t.Run("a non-cursor rule set still suppresses the default", func(t *testing.T) {
		rec := &recorder{}
		srv := serve(rec)
		defer srv.Close()
		// MaxRequestNumber is a declared rule but composes no next request, so the
		// loop reaches the RFC 5988 fallback with `declared` true.
		tbl, _, err := restSrc(t, &API{}, map[string]any{
			"type": "RestSource", "url": srv.URL + "/p1",
			"paginationRules": map[string]any{"MaxRequestNumber": "9"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tbl.Rows) != 1 {
			t.Fatalf("a declared rule set must suppress the Link default: rows=%d", len(tbl.Rows))
		}
	})

	t.Run("an explicit true is honoured alongside other rules", func(t *testing.T) {
		rec := &recorder{}
		srv := serve(rec)
		defer srv.Close()
		// Writing SupportRFC5988 is asking for it, which must outrank the
		// "suppressed because rules were declared" default.
		tbl, _, err := restSrc(t, &API{}, map[string]any{
			"type": "RestSource", "url": srv.URL + "/p1",
			"paginationRules": map[string]any{"MaxRequestNumber": "9", "SupportRFC5988": "true"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tbl.Rows) != 2 {
			t.Fatalf("an explicit SupportRFC5988 must still page: rows=%d", len(tbl.Rows))
		}
	})

	t.Run("SupportRFC5988 false disables it", func(t *testing.T) {
		rec := &recorder{}
		srv := serve(rec)
		defer srv.Close()
		tbl, _, err := restSrc(t, &API{}, map[string]any{
			"type": "RestSource", "url": srv.URL + "/p1",
			"paginationRules": map[string]any{"SupportRFC5988": "false"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tbl.Rows) != 1 {
			t.Fatalf("rows=%d, want the Link header ignored", len(tbl.Rows))
		}
	})
}

func TestPaginationResendsThePostBodyOnEveryPage(t *testing.T) {
	// An io.Reader is consumed by the first request. A second page that silently
	// sent an empty body would be a very quiet bug.
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.log(r)
		if r.URL.Path == "/p2" {
			fmt.Fprint(w, `{"entries":[{"id":2}]}`)
			return
		}
		fmt.Fprintf(w, `{"entries":[{"id":1}],"next":%q}`, srv.URL+"/p2")
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL + "/p1",
		"requestMethod": "POST", "requestBody": `{"q":"state=open"}`,
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.bodies) != 2 || rec.bodies[0] != rec.bodies[1] || rec.bodies[1] == "" {
		t.Fatalf("every page must carry the body: %q", rec.bodies)
	}
}

func TestPaginationRefusesATopLevelArray(t *testing.T) {
	// Fabric states pagination is unsupported for this shape. Accepting it would
	// read exactly one page while looking like it paged.
	srv := jsonServer(t, `[{"a":1}]`)
	_, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL,
		"paginationRules": map[string]any{"AbsoluteUrl": "$.next"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("err = %v, want the array shape named", err)
	}
}

func TestPaginationRefusesRulesItDoesNotImplement(t *testing.T) {
	srv := jsonServer(t, `{"entries":[]}`)
	for _, tc := range []struct{ name, key, val, want string }{
		{"unknown key", "Cursor", "$.next", "not one the emulator implements"},
		{"range on a literal key", "QueryParameters.offset", "RANGE:0::10", "steps a {placeholder}"},
		{"placeholder without a range", "QueryParameters.{o}", "$.next", "must be RANGE"},
		{"bad MaxRequestNumber", "MaxRequestNumber", "lots", "not a positive number"},
		{"zero step", "QueryParameters.{o}", "RANGE:0:10:0", "non-zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := restSrc(t, &API{}, map[string]any{
				"type": "RestSource", "url": srv.URL,
				"paginationRules": map[string]any{tc.key: tc.val},
			}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRFC5988NextParsing(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{`<https://x/2>; rel="next"`}, "https://x/2"},
		{[]string{`<https://x/1>; rel="prev", <https://x/3>; rel="next"`}, "https://x/3"},
		{[]string{`<https://x/1>; rel=prev`, `<https://x/9>; rel=next`}, "https://x/9"},
		{[]string{`<https://x/1>; rel="last"`}, ""},
		{[]string{`garbage`}, ""},
	} {
		got, ok := rfc5988Next(tc.in)
		if tc.want == "" && ok {
			t.Fatalf("rfc5988Next(%v) = %q, want no match", tc.in, got)
		}
		if tc.want != "" && got != tc.want {
			t.Fatalf("rfc5988Next(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
