package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHandlerReadsABodyThroughABareLimitReader is the reason this package
// exists, and it is worth more than the nine fixes it enforces.
//
// Those fixes were nine edits. Without this, the tenth site — written next
// month by someone reaching for the obvious idiom — is silently truncating
// again, and the only signal will be a client somewhere with a short file.
// docs/10-testing.md calls this the difference between fixing a bug and closing
// a class.
//
// The rule: a body is read through httpx.ReadBounded, or the line carries an
// explicit `bounded-read-exempt:` comment saying why it does not need to be.
// Two exemptions exist today and both are real — a body drained to Discard so a
// connection can be reused, and one clipped purely to quote into an error
// message. Neither stores or serves what it read.
const exemptMarker = "bounded-read-exempt:"

// The banned idiom. `io.ReadAll(io.LimitReader(...))` reports clean EOF at the
// ceiling, so the caller cannot tell a body that fitted from one that was cut.
var banned = []string{
	"io.ReadAll(io.LimitReader(r.Body",
	"io.ReadAll(io.LimitReader(req.Body",
	"io.ReadAll(io.LimitReader(resp.Body",
	"io.ReadAll(io.LimitReader(response.Body",
}

func TestNoHandlerReadsABodyThroughABareLimitReader(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	var offenders []string
	scanned := 0
	err = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files may demonstrate the banned idiom while proving it is bad.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			hit := false
			for _, b := range banned {
				if strings.Contains(line, b) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			// The exemption may sit on this line or on any of the three
			// immediately above it, which is where a reason naturally goes.
			exempt := strings.Contains(line, exemptMarker)
			for back := 1; back <= 3 && i-back >= 0 && !exempt; back++ {
				if strings.Contains(lines[i-back], exemptMarker) {
					exempt = true
				}
			}
			if !exempt {
				// ToSlash, because filepath.Rel yields backslashes on Windows
				// and this string is compared and printed on three platforms.
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders,
					filepath.ToSlash(rel)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A walk that found nothing to read would pass this test while checking
	// nothing at all — the failure mode this whole exercise is about.
	if scanned < 50 {
		t.Fatalf("only scanned %d files under internal/; the walk is not reaching the source", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("%d body read(s) through a bare io.LimitReader — it discards the "+
			"excess and reports success, so an oversized body is stored or served "+
			"as a fragment. Use httpx.ReadBounded, or add a `%s <reason>` comment "+
			"if nothing is stored or served:\n  %s",
			len(offenders), exemptMarker, strings.Join(offenders, "\n  "))
	}
}

// TestTheExemptionsAreStillTheOnesWeApproved keeps the escape hatch honest.
//
// An exemption marker silences the guard above, so a growing pile of them would
// dismantle the rule quietly. Two are approved; a third has to be argued for
// here, in front of whoever is reading the diff.
func TestTheExemptionsAreStillTheOnesWeApproved(t *testing.T) {
	root, _ := filepath.Abs("../..")
	var found []string
	_ = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, _ := os.ReadFile(path)
		if n := strings.Count(string(src), exemptMarker); n > 0 {
			// Forward slashes always: the approved list below is written one
			// way, and on Windows filepath.Rel returns `internal\\entra\\...`,
			// so the lookup missed and the test failed there and only there.
			// Caught by CI on windows-latest — a reminder that a guard test is
			// code and gets its own bugs.
			rel, _ := filepath.Rel(root, path)
			for i := 0; i < n; i++ {
				found = append(found, filepath.ToSlash(rel))
			}
		}
		return nil
	})

	want := map[string]int{
		// Drains the body to Discard so the connection can be reused.
		"internal/onelake/onelake.go": 1,
		// Clips a rejected-credentials body purely to quote in an error.
		"internal/entra/client.go": 1,
	}
	got := map[string]int{}
	for _, f := range found {
		got[f]++
	}
	for f, n := range got {
		if want[f] != n {
			t.Fatalf("unapproved bounded-read exemption in %s (%d found, %d approved). "+
				"Adding one is fine, but say here why nothing it reads is stored or served.",
				f, n, want[f])
		}
	}
	for f, n := range want {
		if got[f] != n {
			t.Fatalf("approved exemption in %s is gone (%d found, %d expected) — "+
				"if the code no longer needs it, drop it from this list too", f, got[f], n)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
