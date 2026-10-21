package testsupport_test

import (
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// The format is the one the activity API's bounds are spelled in, and it is a
// DAY with no time component -- which is the whole reason the straddle exists
// to be handled.
//
// THIS ORACLE IS BRACKETED, NOT COMPARED AGAINST A SECOND READ. Comparing
// UTCDay() against one later `time.Now().UTC().Format(...)` is the exact defect
// this helper exists to remove, so the oracle would be flaky in precisely the
// way it is asserting against -- it would fail once a day, in a sub-second
// window, on a test whose subject is that failure. Instead the call is
// surrounded by two reads and EITHER of their days is accepted: a midnight
// crossing inside the bracket is then a pass, and a UTCDay that read LOCAL
// time or a different day still fails. These three reads are the reason this
// function has a docs/clock-test-flakiness.json entry; it is the oracle FOR
// the clock, so it cannot avoid reading one.
func TestUTCDayIsADayAndIsUTC(t *testing.T) {
	start := time.Now().UTC()
	got := testsupport.UTCDay()
	end := time.Now().UTC()

	parsed, err := time.Parse("2006-01-02", got)
	if err != nil {
		t.Fatalf("UTCDay() = %q, not a 2006-01-02 day: %v", got, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("parsed %v in %v, want UTC", parsed, parsed.Location())
	}
	if got != start.Format("2006-01-02") && got != end.Format("2006-01-02") {
		// Reachable only if UTCDay read local time, or a day outside the
		// bracket. Both are the defect; a straddle is not.
		t.Fatalf("UTCDay() = %q, want the UTC day of an instant in [%v, %v]",
			got, start, end)
	}
}

// The guard that actually holds the repair. The checker in scripts/ can only
// see that a day was read; whether the straddle is HANDLED is decided here.
func TestUTCDaysSinceCoversTheStraddleInBothDirections(t *testing.T) {
	today := testsupport.UTCDay()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")

	for _, tc := range []struct {
		name     string
		captured string
		want     []string
	}{
		// The ordinary case: no midnight crossed, one query, and the API's
		// refusal of a two-day window is never approached.
		{"same day", today, []string{today}},
		// Captured just before midnight, queried just after: the events are in
		// the captured day and the union must still hold it. Dropping
		// `captured` here is the mutation that would make the sub-second
		// failure silent again.
		{"day advanced", yesterday, []string{yesterday, today}},
		// The other ordering, which a "widen forwards from captured" fix would
		// get wrong: a clock that has gone BACK (a corrected host clock, or a
		// captured day in the future) still yields two days rather than one,
		// because the comparison is inequality and not ordering.
		{"day went back", tomorrow, []string{tomorrow, today}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := testsupport.UTCDaysSince(tc.captured)
			if len(got) != len(tc.want) {
				t.Fatalf("UTCDaysSince(%q) = %v, want %v", tc.captured, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UTCDaysSince(%q) = %v, want %v", tc.captured, got, tc.want)
				}
			}
		})
	}
}

// Every day returned is one the API will accept as a single-day window. A
// straddle repaired by widening the window instead would fail this: the API
// answers 400 for bounds on two different days, which is what forces the union.
func TestEveryDayReturnedIsASingleDay(t *testing.T) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, day := range testsupport.UTCDaysSince(yesterday) {
		if _, err := time.Parse("2006-01-02", day); err != nil {
			t.Fatalf("UTCDaysSince returned %q, not a single day: %v", day, err)
		}
	}
}
