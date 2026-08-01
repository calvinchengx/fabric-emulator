package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// The VS Code compatibility surface has two auth wrappers, and they were the
// least-covered code in the package despite being security-relevant. Both
// share the same fallback shape — prefer the Power BI validator, fall back to
// the control-plane one — and both must fail closed when neither exists.
//
// `withMWCAuth` additionally requires the `MwcToken` scheme rather than
// `Bearer`: the Fabric VS Code extension sends its workload token that way, so
// accepting `Bearer` here would let a control-plane token through a door it
// was not minted for.

func vscodeHandler() (handler, *bool) {
	called := false
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		called = true
		w.WriteHeader(http.StatusOK)
	}, &called
}

func TestWithMWCAuthRequiresTheMwcTokenScheme(t *testing.T) {
	a := &API{} // no validators configured

	h, called := vscodeHandler()
	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"no authorization at all", "", http.StatusUnauthorized},
		{"bearer is not accepted here", "Bearer abc", http.StatusUnauthorized},
		{"a bare token is not accepted", "abc", http.StatusUnauthorized},
		{"the scheme alone is not a token", "MwcToken", http.StatusUnauthorized},
		// With the scheme present the request gets as far as the validator,
		// which is absent — so it fails closed as not-implemented rather than
		// letting the call through.
		{"scheme present, no validator configured", "MwcToken abc", http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/webapi/x", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			a.withMWCAuth(h)(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if *called {
				t.Fatal("the handler ran despite the request not being authorized")
			}
		})
	}
}

// The scheme match must be case-insensitive — the extension's casing is not
// something the emulator should depend on — but still fail closed afterwards.
func TestWithMWCAuthSchemeIsCaseInsensitive(t *testing.T) {
	a := &API{}
	h, called := vscodeHandler()
	for _, hdr := range []string{"mwctoken abc", "MWCTOKEN abc", "MwCtOkEn abc"} {
		r := httptest.NewRequest("GET", "/webapi/x", nil)
		r.Header.Set("Authorization", hdr)
		w := httptest.NewRecorder()
		a.withMWCAuth(h)(w, r)
		// Past the scheme check, into the absent-validator branch.
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%q: status = %d, want 501 (scheme should have matched)", hdr, w.Code)
		}
		if *called {
			t.Fatalf("%q: handler ran without a validator", hdr)
		}
	}
}

// With no validator at all, the control-plane-or-PBI wrapper must also fail
// closed rather than treating an unauthenticated caller as anonymous.
func TestWithPBIOrControlAuthFailsClosedWithoutAValidator(t *testing.T) {
	a := &API{}
	h, called := vscodeHandler()
	r := httptest.NewRequest("GET", "/webapi/x", nil)
	r.Header.Set("Authorization", "Bearer abc")
	w := httptest.NewRecorder()
	a.withPBIOrControlAuth(h)(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
	if *called {
		t.Fatal("the handler ran with no validator configured")
	}
}

// A validator that rejects the token must produce 401, and must not fall
// through to the handler.
func TestWithPBIOrControlAuthRejectsABadToken(t *testing.T) {
	a := &API{Auth: &auth.Validator{Issuer: "https://example.invalid/v2.0"}}
	h, called := vscodeHandler()
	r := httptest.NewRequest("GET", "/webapi/x", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	w := httptest.NewRecorder()
	a.withPBIOrControlAuth(h)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if *called {
		t.Fatal("the handler ran with an invalid token")
	}
}

// Same for the MwcToken wrapper: a well-formed scheme with a bad token is 401.
func TestWithMWCAuthRejectsABadToken(t *testing.T) {
	a := &API{Auth: &auth.Validator{Issuer: "https://example.invalid/v2.0"}}
	h, called := vscodeHandler()
	r := httptest.NewRequest("GET", "/webapi/x", nil)
	r.Header.Set("Authorization", "MwcToken not-a-jwt")
	w := httptest.NewRecorder()
	a.withMWCAuth(h)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if *called {
		t.Fatal("the handler ran with an invalid token")
	}
}
