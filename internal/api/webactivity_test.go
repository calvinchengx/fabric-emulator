package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Web activity is the one leaf whose whole value is the side effect, so
// every case here drives a REAL server. A test that asserted the shape of a
// fabricated response would reproduce the bug this replaced.

// act builds a Web activity with the given typeProperties.
func webAct(t *testing.T, name string, tp map[string]any) (pipeline.Activity, map[string]json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(tp)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return pipeline.Activity{Name: name, Type: "WebActivity", TypeProperties: raw}, fields
}

// literal resolves a value the way the interpreter would for a non-expression.
func literal(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func execWeb(t *testing.T, api *API, tp map[string]any) (map[string]any, error) {
	t.Helper()
	e := &pipelineExecutor{a: api}
	act, fields := webAct(t, "Call", tp)
	return e.webActivity(act, fields, literal)
}

func TestWebActivityReallyCalls(t *testing.T) {
	var got struct {
		method string
		path   string
		auth   string
		body   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.auth, got.body = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","count":2}`))
	}))
	defer srv.Close()

	out, err := execWeb(t, &API{}, map[string]any{
		"url":     srv.URL + "/hook",
		"method":  "POST",
		"headers": map[string]any{"Authorization": "Bearer t0ken"},
		"body":    map[string]any{"hello": "world"},
	})
	if err != nil {
		t.Fatalf("web activity failed: %v", err)
	}

	// The request the server actually received — this is the assertion the old
	// stub could never make.
	if got.method != "POST" || got.path != "/hook" {
		t.Fatalf("server saw %s %s, want POST /hook", got.method, got.path)
	}
	if got.auth != "Bearer t0ken" {
		t.Fatalf("Authorization header not sent: %q", got.auth)
	}
	if got.body != `{"hello":"world"}` {
		t.Fatalf("body = %q", got.body)
	}

	// A JSON object is merged at the top level, so a downstream
	// `@activity('Call').output.id` resolves.
	if out["id"] != "abc" {
		t.Fatalf("response body not merged into output: %#v", out)
	}
	if out["status"] != "Succeeded" || out["statusCode"] != 200 {
		t.Fatalf("status/statusCode = %v/%v", out["status"], out["statusCode"])
	}
	if _, ok := out[webHeadersKey].(map[string]any); !ok {
		t.Fatalf("no %s in output: %#v", webHeadersKey, out)
	}
}

func TestWebActivityDefaultsToGETWithNoBody(t *testing.T) {
	var sawBody, sawCT string
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sawBody, sawCT, method = string(b), r.Header.Get("Content-Type"), r.Method
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := execWeb(t, &API{}, map[string]any{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if method != "GET" {
		t.Fatalf("method = %q, want GET", method)
	}
	// No body means no Content-Type: a GET announcing application/json with
	// nothing in it misleads a server that switches on it.
	if sawBody != "" || sawCT != "" {
		t.Fatalf("unexpected body %q / content-type %q", sawBody, sawCT)
	}
}

func TestWebActivityFailsOnNon2xx(t *testing.T) {
	// Fabric fails the activity rather than handing the status back, so a
	// pipeline cannot accidentally treat a 500 as success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	_, err := execWeb(t, &API{}, map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatal("a 500 must fail the activity")
	}
	// The message has to carry the status AND the body: "activity failed" with
	// neither is the report that sends someone to the server's own logs.
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error does not say what happened: %v", err)
	}
}

func TestWebActivityCarriesANonJSONBodyUnparsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	}))
	defer srv.Close()

	out, err := execWeb(t, &API{}, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if out["body"] != "plain text" {
		t.Fatalf("non-JSON body not carried: %#v", out)
	}
}

func TestWebActivityRefusesAMissingOrNonHTTPURL(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"absent", "", "url is required"},
		{"scheme", "file:///etc/passwd", "not http(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := map[string]any{}
			if tc.url != "" {
				tp["url"] = tc.url
			}
			_, err := execWeb(t, &API{}, tp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWebActivityRefusesAnOversizedResponse(t *testing.T) {
	// The output lives in memory and in the run record; a Web activity is not
	// a download mechanism, and the refusal says so.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, webMaxBody+16))
	}))
	defer srv.Close()

	_, err := execWeb(t, &API{}, map[string]any{"url": srv.URL})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an exceeds-limit refusal", err)
	}
}

func TestWebActivityHonoursThePolicyTimeout(t *testing.T) {
	// A hung endpoint must fail the activity at its declared budget rather than
	// holding the pipeline open. 1 second, so the test costs a second at worst.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	e := &pipelineExecutor{a: &API{}}
	act, fields := webAct(t, "Hang", map[string]any{"url": srv.URL})
	act.Policy = &pipeline.Policy{Timeout: "00:00:01"}

	_, err := e.webActivity(act, fields, literal)
	if err == nil {
		t.Fatal("a hung endpoint must fail the activity, not hang the pipeline")
	}
}

func TestWebActivityStubModeSaysItDidNotCall(t *testing.T) {
	// The escape hatch for a hermetic CI leg. It must be OBVIOUS in the output:
	// an unlabelled fake success is what this whole change removed.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	out, err := execWeb(t, &API{WebActivityStub: true}, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("stub mode called the server")
	}
	if out["stubbed"] != true {
		t.Fatalf("a stubbed run must say so: %#v", out)
	}
}

func TestWebActivityResolvesExpressions(t *testing.T) {
	// url/headers/body all go through the interpreter's resolver — building a
	// URL from a pipeline parameter is the reason this activity exists.
	var sawPath, sawHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawHeader = r.URL.Path, r.Header.Get("X-Run")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := &pipelineExecutor{a: &API{}}
	act, fields := webAct(t, "Call", map[string]any{
		"url":     "@{concat('" + srv.URL + "','/v1/items')}",
		"headers": map[string]any{"X-Run": "@{pipeline().RunId}"},
	})
	// A resolver standing in for the interpreter's: it substitutes the two
	// expressions this activity carries.
	resolve := func(raw json.RawMessage) (any, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return literal(raw)
		}
		switch {
		case strings.Contains(s, "concat("):
			return srv.URL + "/v1/items", nil
		case strings.Contains(s, "RunId"):
			return "run-42", nil
		}
		return s, nil
	}
	if _, err := e.webActivity(act, fields, resolve); err != nil {
		t.Fatal(err)
	}
	if sawPath != "/v1/items" {
		t.Fatalf("url expression not resolved: path = %q", sawPath)
	}
	if sawHeader != "run-42" {
		t.Fatalf("header expression not resolved: %q", sawHeader)
	}
}

func TestWebActivityReportsAnUnreachableHost(t *testing.T) {
	// A connection failure is the activity's failure, and the message must name
	// the method and URL — "connection refused" alone does not say which of a
	// pipeline's several Web activities it was.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := execWeb(t, &API{}, map[string]any{"url": url})
	if err == nil {
		t.Fatal("an unreachable host must fail the activity")
	}
	if !strings.Contains(err.Error(), "Call") || !strings.Contains(err.Error(), url) {
		t.Fatalf("error names neither the activity nor the url: %v", err)
	}
}

func TestWebActivitySendsAStringBodyVerbatim(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		saw = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := execWeb(t, &API{}, map[string]any{
		"url": srv.URL, "method": "POST", "body": "id,name\n1,ada",
	}); err != nil {
		t.Fatal(err)
	}
	// Not re-encoded as a JSON string: an author who wrote raw text meant it.
	if saw != "id,name\n1,ada" {
		t.Fatalf("string body was transformed: %q", saw)
	}
}

func TestParseTimeoutRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		in   string
		secs float64
		ok   bool
	}{
		{"00:00:30", 30, true},
		{"01:00:00", 3600, true},
		{"1.00:00:00", 86400, true},
		{"", 0, false},
		{"nonsense", 0, false},
	} {
		d, ok := pipeline.ParseTimeout(tc.in)
		if ok != tc.ok {
			t.Fatalf("ParseTimeout(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
		if ok && d.Seconds() != tc.secs {
			t.Fatalf("ParseTimeout(%q) = %v, want %v", tc.in, d, fmt.Sprintf("%vs", tc.secs))
		}
	}
}
