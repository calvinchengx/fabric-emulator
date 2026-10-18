package onelake

// SAS redaction for the request traces.
//
// THE FINDING (docs/64-logging-quality.md, F7). The FABRIC_ONELAKE_TRACE lines
// in onelake.go and blob.go printed `r.URL.RawQuery` verbatim, plus the
// `x-ms-copy-source` and `x-ms-rename-source` headers. All three carry an Azure
// shared access signature in ordinary use: a SAS IS a query string (`?sv=…&se=…
// &sp=…&sig=…`), a copy source is by convention a full SAS URL, and
// renameSource's own comment says the header "may carry a sas/query suffix".
// `sig` is the HMAC -- possession of it is possession of the access it grants.
//
// THE ASYMMETRY IS INSIDE THIS ONE REPOSITORY, which is what makes it a finding
// rather than a preference. internal/server/record.go refuses to record
// response headers AT ALL, and says why: "the Authorization header carries a
// bearer token, and a recording written to a file that CI may upload as an
// artifact is exactly the place a token should never be." The trace one package
// over printed the same class of credential to stderr, which compose captures
// and CI uploads by the same route.
//
// REDACT THE VALUE, KEEP THE NAME, so the trace stays worth having. A reader
// still sees that a SAS was presented and which parameters it carried -- which
// is most of what the trace is for, since a missing `se` or a wrong `sp` is the
// usual reason a storage client is refused. Replacing the whole query with one
// token would have answered "a SAS, probably" where it now answers "this SAS,
// minus the secret".
//
// WHAT THIS IS NOT. It is not a general secret scrubber and it does not try to
// be: it knows the Azure SAS parameter names and nothing else. A credential
// arriving under some other name in some other field goes through untouched --
// the same limit scripts/check_logging_quality.py's R5 documents about itself,
// and the reason R5 is a convention gate rather than a guarantee.

import "strings"

// sasParams are the query parameters an Azure shared access signature is made
// of. The service SAS, account SAS and user-delegation SAS between them use all
// of these (rest/api/storageservices: "Create a service SAS", "Create an account
// SAS", "Create a user delegation SAS").
//
// THE WHOLE SET, NOT JUST `sig`. The signature alone is the secret, but a SAS is
// only usable as a set: `sig` signs the other parameters, so a reader holding
// everything except `sig` holds an unforgeable description while a reader holding
// `sig` plus a log line carrying the rest holds a working credential. Redacting
// one field and printing its signed inputs beside it would be a redaction in
// name only.
var sasParams = map[string]bool{
	// Core service/account SAS.
	"sig": true, "sv": true, "se": true, "st": true, "sp": true,
	"sr": true, "si": true, "spr": true, "ss": true, "srt": true,
	"sip": true, "sdd": true, "saoid": true, "suoid": true, "scid": true,
	// User-delegation SAS: the key the signature was derived from.
	"skoid": true, "sktid": true, "ske": true, "skt": true,
	"sks": true, "skv": true,
	// The response headers a SAS may override. Not secret, but they are signed
	// by `sig`, so they belong to the set for the reason above.
	"rscc": true, "rscd": true, "rsce": true, "rscl": true, "rsct": true,
}

// redactedValue is what replaces a SAS parameter's value. Spelled in capitals
// so a reader can tell at a glance that the trace withheld something rather
// than that the client sent something odd.
const redactedValue = "REDACTED"

// redactSAS replaces the value of every SAS-shaped parameter in a raw query,
// leaving parameter names, order and every other parameter verbatim.
//
// OPERATES ON THE RAW TEXT rather than going through url.ParseQuery and
// Encode(), deliberately. Encode sorts keys and re-escapes values, so a
// round-trip would print a query the client did not send -- and the trace exists
// to show what the client sent. Splitting on the separators preserves the line
// as a faithful record of everything that is not a credential.
func redactSAS(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	// `&` is the separator every Azure client uses; `;` is accepted because Go's
	// own net/url historically did and a trace should not be the thing that
	// disagrees about it.
	var out strings.Builder
	out.Grow(len(rawQuery))
	start := 0
	for i := 0; i <= len(rawQuery); i++ {
		if i < len(rawQuery) && rawQuery[i] != '&' && rawQuery[i] != ';' {
			continue
		}
		out.WriteString(redactParam(rawQuery[start:i]))
		if i < len(rawQuery) {
			out.WriteByte(rawQuery[i])
		}
		start = i + 1
	}
	return out.String()
}

// redactParam redacts one `key=value` pair, or returns it unchanged.
//
// A VALUELESS PARAMETER IS LEFT ALONE: `?sig` with no `=` carries no secret,
// and rewriting it to `sig=REDACTED` would invent an assignment the client did
// not make. A parameter with an EMPTY value is likewise left alone -- `se=` is
// a client bug worth seeing, and REDACTED there would hide it.
func redactParam(pair string) string {
	key, _, ok := strings.Cut(pair, "=")
	if !ok {
		return pair
	}
	if !sasParams[strings.ToLower(key)] {
		return pair
	}
	if len(key) == len(pair)-1 {
		return pair // `key=` with nothing after it
	}
	return key + "=" + redactedValue
}

// redactSASURL redacts the SAS in a URL-valued header -- `x-ms-copy-source`,
// which is by convention a full SAS URL, and `x-ms-rename-source`, whose own
// handler comment notes it "may carry a sas/query suffix".
//
// SPLIT ON THE FIRST `?` RATHER THAN url.Parse. Parse fails on a malformed URL
// and would then leave the caller choosing between printing the raw value and
// printing nothing -- and a malformed copy-source is exactly the case the trace
// is being read for. Everything before the `?` is the account, container and
// blob path, which is what a reader needs; everything after it goes through
// redactSAS.
func redactSASURL(raw string) string {
	if raw == "" {
		return ""
	}
	base, query, ok := strings.Cut(raw, "?")
	if !ok {
		return raw
	}
	return base + "?" + redactSAS(query)
}
