package awssig

import (
	"net/http"
	"testing"
	"time"
)

// AWS publishes a worked example for signing a GET Object request (Signature
// Version 4, "Example: GET Object"). Reproducing its signature byte-for-byte
// is the only real proof the implementation is correct — a self-consistent
// test would pass with the algorithm subtly wrong.
func TestSignMatchesAWSPublishedExample(t *testing.T) {
	req, err := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")

	Sign(req, Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}, "us-east-1", "s3", EmptyPayloadHash,
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))

	const want = "AWS4-HMAC-SHA256 " +
		"Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization mismatch\n got: %s\nwant: %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

// A session token must be signed as x-amz-security-token, or STS credentials
// are rejected.
func TestSignIncludesSessionToken(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://b.s3.amazonaws.com/k", nil)
	Sign(req, Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOKEN"},
		"us-east-1", "s3", EmptyPayloadHash, time.Unix(0, 0))
	if req.Header.Get("x-amz-security-token") != "TOKEN" {
		t.Fatal("session token not set")
	}
	if got := req.Header.Get("Authorization"); !contains(got, "x-amz-security-token") {
		t.Fatalf("session token not signed: %s", got)
	}
}

// Query parameters are sorted and RFC3986-escaped; spaces must not become '+'.
func TestCanonicalQueryEncoding(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://b.s3.amazonaws.com/k?b=two+words&a=1&c=~x", nil)
	q := canonicalQuery(req.URL)
	if q != "a=1&b=two%20words&c=~x" {
		t.Fatalf("canonicalQuery = %q", q)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// HashPayload is what SigV4 signs over for a request with a body. It is not
// reachable through the shortcut write path (S3 shortcuts are read-only, so a
// write is refused before signing), so it is pinned directly.
func TestHashPayload(t *testing.T) {
	// SHA256("") is the documented empty-payload constant.
	if got := HashPayload(nil); got != EmptyPayloadHash {
		t.Fatalf("HashPayload(nil) = %s, want the empty-payload constant", got)
	}
	if got := HashPayload([]byte{}); got != EmptyPayloadHash {
		t.Fatalf("HashPayload(empty) = %s", got)
	}
	// A known vector: SHA256("abc").
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashPayload([]byte("abc")); got != abc {
		t.Fatalf("HashPayload(\"abc\") = %s, want %s", got, abc)
	}
	// Different bodies must not collide.
	if HashPayload([]byte("a")) == HashPayload([]byte("b")) {
		t.Fatal("distinct payloads hashed the same")
	}
}

// A signed request with a body carries that body's hash, not the empty one —
// signing a PUT with the empty-payload hash is rejected by real S3.
func TestSignUsesBodyHash(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://b.s3.amazonaws.com/k", nil)
	body := []byte("payload bytes")
	Sign(req, Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"},
		"us-east-1", "s3", HashPayload(body), time.Unix(0, 0))
	if got := req.Header.Get("x-amz-content-sha256"); got != HashPayload(body) {
		t.Fatalf("x-amz-content-sha256 = %s, want the body hash", got)
	}
	if req.Header.Get("x-amz-content-sha256") == EmptyPayloadHash {
		t.Fatal("a body-carrying request was signed with the empty-payload hash")
	}
}
