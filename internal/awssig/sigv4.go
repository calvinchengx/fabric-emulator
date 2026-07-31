// Package awssig implements AWS Signature Version 4 for outbound requests.
//
// OneLake shortcuts to Amazon S3 (and S3-compatible endpoints) authenticate
// with an Access Key ID and Secret Access Key — that is what Fabric's own
// shortcut dialogs collect (fabric-docs onelake/create-s3-shortcut.md and
// create-s3-compatible-shortcut.md). Talking to a real S3 server therefore
// means signing, not sending a bearer or basic header.
//
// Scope: what the shortcut read-through needs — GET/HEAD with no body, header
// authorization (not presigned URLs), single chunk. The algorithm is
// specified by AWS; this is a direct implementation of it, with a test
// against AWS's own published example.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmptyPayloadHash is SHA256(""), the payload hash for a body-less request.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const algorithm = "AWS4-HMAC-SHA256"

// Credentials identify the caller to S3.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is optional (STS); when set it is signed as
	// x-amz-security-token, which is what AWS requires.
	SessionToken string
}

// Sign adds the Authorization, X-Amz-Date and x-amz-content-sha256 headers to
// req. payloadHash is the hex SHA256 of the body — use EmptyPayloadHash for a
// body-less request. now must be UTC.
func Sign(req *http.Request, creds Credentials, region, service, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	// The Host header is signed, but net/http keeps it on the URL rather than
	// in Header, so it has to be added to the set explicitly.
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	signedHeaders, canonicalHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algorithm, amzDate, scope, hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, dateStamp, region, service), stringToSign))
	req.Header.Set("Authorization", algorithm+
		" Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// canonicalHeaders returns the signed-header list and the canonical header
// block. Host is always signed; so is every x-amz-* header.
func canonicalHeaders(req *http.Request) (signed string, canonical string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	values := map[string]string{"host": host}
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		// host and every x-amz-* header must be signed. content-type and range
		// are signed too: range because the shortcut read-through forwards
		// ranged GETs, and AWS's own worked example signs it.
		if lower == "host" || strings.HasPrefix(lower, "x-amz-") ||
			lower == "content-type" || lower == "range" {
			// Sequential whitespace collapses to one space, per the spec.
			values[lower] = strings.Join(strings.Fields(strings.Join(vs, ",")), " ")
		}
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalURI is the path, URI-encoded per segment. S3 does NOT double-encode
// the path (unlike most other AWS services), so each segment is encoded once.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery sorts parameters by name and encodes them.
func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, rfc3986Escape(k)+"="+rfc3986Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// rfc3986Escape encodes per the signing spec: url.QueryEscape is close but
// encodes a space as '+' and leaves '+' alone, both of which break the
// signature.
func rfc3986Escape(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	return strings.ReplaceAll(e, "%7E", "~")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signingKey derives the date/region/service-scoped key.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}
