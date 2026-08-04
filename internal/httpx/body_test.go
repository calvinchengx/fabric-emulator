package httpx

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// endless yields zeroes forever, so a body of any size costs no memory.
type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }

// failing errors partway through, which a caller must treat exactly as it
// treats an oversized body: what arrived is not what was sent.
type failing struct{ afterN int }

func (f *failing) Read(p []byte) (int, error) {
	if f.afterN <= 0 {
		return 0, errors.New("connection reset")
	}
	n := min(len(p), f.afterN)
	f.afterN -= n
	return n, nil
}

func TestReadBoundedAtEveryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		max  int64
		ok   bool
	}{
		{"empty", 0, 10, true},
		{"under", 9, 10, true},
		// The case the whole helper is for. `io.LimitReader(r, 10)` on an
		// 11-byte body returns 10 bytes and a nil error, indistinguishable
		// from a body that genuinely was 10.
		{"exactly at the ceiling", 10, 10, true},
		{"one byte over", 11, 10, false},
		{"far over", 1 << 20, 10, false},
		{"a zero ceiling accepts only an empty body", 0, 0, true},
		{"a zero ceiling rejects one byte", 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, ok := ReadBounded(io.LimitReader(endless{}, tc.size), tc.max)
			if ok != tc.ok {
				t.Fatalf("ReadBounded(%d bytes, max %d) ok = %v; want %v",
					tc.size, tc.max, ok, tc.ok)
			}
			if !ok && data != nil {
				t.Fatalf("a refused read returned %d bytes; it must return none, "+
					"or the truncation grows back one caller at a time", len(data))
			}
			if ok && int64(len(data)) != tc.size {
				t.Fatalf("returned %d bytes; want %d", len(data), tc.size)
			}
		})
	}
}

func TestAReadErrorIsRefusedLikeAnOversizedBody(t *testing.T) {
	// A caller cannot act on half a body regardless of WHY it is half.
	data, ok := ReadBounded(&failing{afterN: 4}, 1<<20)
	if ok {
		t.Fatal("a body that errored mid-read was accepted")
	}
	if data != nil {
		t.Fatalf("a failed read returned %d bytes", len(data))
	}
}

func TestTheContentIsReturnedIntactNotJustItsLength(t *testing.T) {
	// Length-only assertions would pass on a helper that returned the right
	// number of wrong bytes.
	want := strings.Repeat("fabric", 1000)
	got, ok := ReadBounded(strings.NewReader(want), int64(len(want)))
	if !ok || string(got) != want {
		t.Fatalf("ReadBounded round trip failed (ok=%v, %d bytes)", ok, len(got))
	}
}

func TestEveryCeilingIsPositiveAndOrdered(t *testing.T) {
	// A ceiling accidentally left at 0 would refuse every non-empty body, and
	// the symptom — everything 413s — is far enough from the cause to be worth
	// one line of arithmetic here.
	for name, v := range map[string]int64{
		"MaxDFSAppend": MaxDFSAppend, "MaxDFSPut": MaxDFSPut,
		"MaxBlobWrite": MaxBlobWrite, "MaxBlobMetadata": MaxBlobMetadata,
		"MaxItemContent": MaxItemContent, "MaxProxyBody": MaxProxyBody,
		"MaxControlBody": MaxControlBody, "MaxExternalRead": MaxExternalRead,
	} {
		if v <= 0 {
			t.Fatalf("%s = %d; a ceiling must be positive", name, v)
		}
	}
	// ADLS Gen2 documents 100 MiB for a single append; drifting below that
	// would reintroduce the exact failure `fab cp` hit.
	if MaxDFSAppend < 100<<20 {
		t.Fatalf("MaxDFSAppend = %d; ADLS Gen2 accepts 100 MiB per append", int64(MaxDFSAppend))
	}
}
