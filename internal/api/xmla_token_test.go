package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// The XMLA MWC token is a credential. It used to be one fixed string, compiled
// into the binary and attributed to whoever had exchanged most recently: anyone
// could send it with no Entra token at all, and two real callers traded
// identities. Each test below fails against that implementation.

// exchange runs generateastoken as p and returns the token it issued.
func exchange(t *testing.T, a *API, p *auth.Principal) string {
	t.Helper()
	w := do(a.xmlaToken, p, "POST", `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("generateastoken as %s = %d %s", p.ID, w.Code, w.Body)
	}
	var body struct{ Token string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Token == "" {
		t.Fatalf("generateastoken reply %s: %v", w.Body, err)
	}
	return body.Token
}

// callAs presents header on an XMLA route and reports who the handler ran as,
// or nil when the handler was never reached.
func callAs(a *API, header string) (*auth.Principal, int) {
	var got *auth.Principal
	h := a.withXMLAAuth(func(w http.ResponseWriter, _ *http.Request, p *auth.Principal) {
		got = p
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest("POST", "/webapi/xmla", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return got, w.Code
}

// The identity swap. An Admin exchanges, then a Viewer does: the Admin's token
// must still run as the Admin, not as whoever exchanged last.
func TestEachMWCTokenRunsAsTheCallerItWasIssuedTo(t *testing.T) {
	a, _ := newAPI(t)
	adminTok := exchange(t, a, admin)
	viewerTok := exchange(t, a, viewer)

	if adminTok == viewerTok {
		t.Fatal("two callers were issued the same token")
	}
	for _, tc := range []struct {
		tok  string
		want *auth.Principal
	}{{adminTok, admin}, {viewerTok, viewer}, {adminTok, admin}} {
		if got, _ := callAs(a, "MwcToken "+tc.tok); got == nil || got.ID != tc.want.ID {
			t.Fatalf("token issued to %s ran as %v", tc.want.ID, got)
		}
	}
	// The Bearer spelling of the same token is honoured the same way.
	if got, _ := callAs(a, "Bearer "+viewerTok); got == nil || got.ID != viewer.ID {
		t.Fatalf("Bearer-spelled token ran as %v", got)
	}
}

// The bypass. The string that used to be the only token authorises nothing,
// even after a real exchange has happened — and neither does no token at all.
func TestTheFormerFixedTokenAuthorisesNothing(t *testing.T) {
	a, _ := newAPI(t)
	exchange(t, a, admin)
	for _, header := range []string{"MwcToken fabric-emulator-mwc-token", "MwcToken mwc_" + strings.Repeat("0", 64), ""} {
		if got, code := callAs(a, header); got != nil || code == http.StatusOK {
			t.Errorf("%q reached the handler as %v (status %d)", header, got, code)
		}
	}
}

// Tokens are unguessable in shape: long, random, and distinct per exchange.
func TestMWCTokensAreDistinctAndCarryEntropy(t *testing.T) {
	a, _ := newAPI(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok := exchange(t, a, admin)
		if seen[tok] {
			t.Fatalf("token %s issued twice", tok)
		}
		seen[tok] = true
		if !strings.HasPrefix(tok, "mwc_") || len(tok) != len("mwc_")+2*mwcTokenBytes {
			t.Fatalf("token %q is not a %d-byte hex token", tok, mwcTokenBytes)
		}
	}
}

// A token stops working when its lifetime ends, on the emulator's own clock.
func TestAnMWCTokenExpires(t *testing.T) {
	a, _ := newAPI(t)
	tok := exchange(t, a, admin)
	a.Store.Clock.Advance(mwcTTL - 1)
	if got, _ := callAs(a, "MwcToken "+tok); got == nil {
		t.Fatal("a token was refused before its lifetime ended")
	}
	a.Store.Clock.Advance(1)
	if got, _ := callAs(a, "MwcToken "+tok); got != nil {
		t.Fatal("an expired token still authorised a call")
	}
}

// Expired tokens are dropped when a new one is issued, so the set is bounded by
// the tokens live at once rather than by every exchange ever made.
func TestExpiredMWCTokensArePruned(t *testing.T) {
	var m mwcTokens
	if _, err := m.issue(admin, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.issue(viewer, mwcTTL); err != nil {
		t.Fatal(err)
	}
	if n := len(m.issued); n != 1 {
		t.Fatalf("%d tokens held after the first expired, want 1", n)
	}
}

// No randomness, no token: a predictable token is the bug this replaced.
func TestNoMWCTokenIsIssuedWithoutRandomness(t *testing.T) {
	a, _ := newAPI(t)
	orig := mwcRand
	mwcRand = func([]byte) (int, error) { return 0, errors.New("entropy exhausted") }
	t.Cleanup(func() { mwcRand = orig })
	w := do(a.xmlaToken, admin, "POST", `{}`, nil)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"Token"`) {
		t.Fatalf("= %d %s, want a 500 and no token", w.Code, w.Body)
	}
}

// Two API instances do not share tokens: the state belongs to the server that
// issued it, not to the process.
func TestMWCTokensBelongToTheServerThatIssuedThem(t *testing.T) {
	a, _ := newAPI(t)
	b, _ := newAPI(t)
	tok := exchange(t, a, admin)
	if got, _ := callAs(b, "MwcToken "+tok); got != nil {
		t.Fatal("a token issued by one server authorised a call on another")
	}
}
