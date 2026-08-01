package schedule

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *Config {
	t.Helper()
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s) = %v", raw, err)
	}
	return c
}

// utc is a compact way to write an expected instant.
func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// fired renders occurrences as RFC3339-UTC for readable failure messages.
func fired(times []time.Time) []string {
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.UTC().Format(time.RFC3339))
	}
	return out
}

func eq(t *testing.T, got []time.Time, want ...string) {
	t.Helper()
	g := fired(got)
	if strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("occurrences =\n  %v\nwant\n  %v", g, want)
	}
}

// ---- validation ----

func TestParseRejectsEveryDocumentedBound(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"not json", `{`, "not valid JSON"},
		{"no type", `{"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "type is required"},
		{"unknown type", `{"type":"Hourly","startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "not one of Cron"},
		{"no zone", `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00"}`, "localTimeZoneId is required"},
		{"bad zone", `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"Middle Earth Standard Time"}`, "not a time zone"},
		{"no start", `{"type":"Cron","interval":60,"endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "startDateTime is required"},
		{"bad start", `{"type":"Cron","interval":60,"startDateTime":"yesterday","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "not an ISO-8601"},
		{"no end", `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00","localTimeZoneId":"UTC"}`, "endDateTime is required"},
		{"end before start", `{"type":"Cron","interval":60,"startDateTime":"2024-02-01T00:00:00","endDateTime":"2024-01-01T00:00:00","localTimeZoneId":"UTC"}`, "must be after startDateTime"},
		{"interval zero", `{"type":"Cron","interval":0,"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "interval must be between"},
		{"interval too big", `{"type":"Cron","interval":5270401,"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "interval must be between"},
		{"cron with times", `{"type":"Cron","interval":60,"times":["09:00"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "does not apply to a Cron"},
		{"daily no times", `{"type":"Daily","startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "times is required"},
		{"daily bad time", `{"type":"Daily","times":["25:00"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "not a valid hh:mm"},
		{"daily unsplit time", `{"type":"Daily","times":["0900"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "is not hh:mm"},
		{"daily with weekdays", `{"type":"Daily","times":["09:00"],"weekdays":["Monday"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "does not apply to a Daily"},
		{"weekly no weekdays", `{"type":"Weekly","times":["09:00"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "weekdays is required"},
		{"weekly bad weekday", `{"type":"Weekly","times":["09:00"],"weekdays":["Caturday"],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "not a weekday name"},
		{"weekly with occurrence", `{"type":"Weekly","times":["09:00"],"weekdays":["Monday"],"occurrence":{"type":"DayOfMonth","dayOfMonth":1},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`, "does not apply to a Weekly"},
		{"monthly no occurrence", `{"type":"Monthly","times":["09:00"],"recurrence":1,"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "occurrence is required"},
		{"monthly no occurrence type", `{"type":"Monthly","times":["09:00"],"recurrence":1,"occurrence":{},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "occurrence.type is required"},
		{"monthly bad occurrence type", `{"type":"Monthly","times":["09:00"],"recurrence":1,"occurrence":{"type":"Fortnightly"},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "not one of DayOfMonth"},
		{"monthly bad day", `{"type":"Monthly","times":["09:00"],"recurrence":1,"occurrence":{"type":"DayOfMonth","dayOfMonth":32},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "dayOfMonth must be between"},
		{"monthly bad weekIndex", `{"type":"Monthly","times":["09:00"],"recurrence":1,"occurrence":{"type":"OrdinalWeekday","weekIndex":"Sixth","weekday":"Tuesday"},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "weekIndex"},
		{"monthly bad ordinal weekday", `{"type":"Monthly","times":["09:00"],"recurrence":1,"occurrence":{"type":"OrdinalWeekday","weekIndex":"Third","weekday":"Blursday"},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "occurrence.weekday"},
		{"monthly recurrence 0", `{"type":"Monthly","times":["09:00"],"recurrence":0,"occurrence":{"type":"DayOfMonth","dayOfMonth":1},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "recurrence must be between"},
		{"monthly recurrence 13", `{"type":"Monthly","times":["09:00"],"recurrence":13,"occurrence":{"type":"DayOfMonth","dayOfMonth":1},"startDateTime":"2024-01-01T00:00:00","endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"UTC"}`, "recurrence must be between"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			if err == nil {
				t.Fatalf("accepted %s", tc.raw)
			}
			var se *Error
			if !asError(err, &se) {
				t.Fatalf("error is not a *schedule.Error: %T", err)
			}
			if se.Code != "InvalidRequest" {
				t.Fatalf("code = %q", se.Code)
			}
			if !strings.Contains(se.Message, tc.want) {
				t.Fatalf("message %q does not mention %q", se.Message, tc.want)
			}
		})
	}
}

// asError is errors.As without importing errors for one call site.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestTooManyTimesAndWeekdaysRejected(t *testing.T) {
	times := make([]string, 0, MaxTimes+1)
	for i := 0; i <= MaxTimes; i++ {
		times = append(times, `"00:00"`) // duplicates: the cap is on entries, not distinct values
	}
	raw := `{"type":"Daily","times":[` + strings.Join(times, ",") +
		`],"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("101 times = %v", err)
	}
	raw = `{"type":"Weekly","times":["09:00"],"weekdays":["Monday","Tuesday","Wednesday","Thursday","Friday","Saturday","Sunday","Monday"],` +
		`"startDateTime":"2024-01-01T00:00:00","endDateTime":"2024-02-01T00:00:00","localTimeZoneId":"UTC"}`
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "at most 7") {
		t.Fatalf("8 weekdays = %v", err)
	}
}

func TestLoadLocationAcceptsWindowsAndIANA(t *testing.T) {
	for _, id := range []string{"UTC", "Pacific Standard Time", "  tokyo standard time ", "America/New_York", "Asia/Kolkata"} {
		if _, err := LoadLocation(id); err != nil {
			t.Fatalf("LoadLocation(%q) = %v", id, err)
		}
	}
	if _, err := LoadLocation(""); err == nil {
		t.Fatal("empty zone accepted")
	}
	if _, err := LoadLocation("Not/AZone"); err == nil {
		t.Fatal("bogus zone accepted")
	}
	// The Windows id must resolve to the same zone as its IANA name, or the
	// mapping is decorative.
	win, _ := LoadLocation("Pacific Standard Time")
	iana, _ := LoadLocation("America/Los_Angeles")
	if win.String() != iana.String() {
		t.Fatalf("Pacific Standard Time → %s, want %s", win, iana)
	}
}

func TestStartDateTimeAcceptedFormats(t *testing.T) {
	// A bare local date-time is read in the schedule's zone…
	c := mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-06-01T09:00:00",
		"endDateTime":"2025-01-01T00:00:00","localTimeZoneId":"Asia/Singapore"}`)
	if got := c.Start().UTC().Format(time.RFC3339); got != "2024-06-01T01:00:00Z" {
		t.Fatalf("local start = %s, want 01:00Z (SGT is UTC+8)", got)
	}
	// …while an explicit offset is honoured as sent.
	c = mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-06-01T09:00:00Z",
		"endDateTime":"2025-01-01T00:00:00Z","localTimeZoneId":"Asia/Singapore"}`)
	if got := c.Start().UTC().Format(time.RFC3339); got != "2024-06-01T09:00:00Z" {
		t.Fatalf("zulu start = %s", got)
	}
	// Date-only and hh:mm forms parse too.
	mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-06-01","endDateTime":"2024-06-02T00:00","localTimeZoneId":"UTC"}`)
}

// ---- Cron ----

func TestCronOccurrences(t *testing.T) {
	c := mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00Z",
		"endDateTime":"2024-01-02T00:00:00Z","localTimeZoneId":"UTC"}`)

	// A never-fired schedule whose start is already behind the clock fires the
	// start instant itself — the documented "starts in the past → triggers
	// instantly" behaviour.
	got, trunc := c.Occurrences(c.Start().Add(-time.Second), utc("2024-01-01T00:30:00Z"))
	eq(t, got, "2024-01-01T00:00:00Z")
	if trunc {
		t.Fatal("truncated")
	}

	// Three hours of clock advance is three more runs, and the window is
	// half-open at the bottom so the instant already fired is not repeated.
	got, _ = c.Occurrences(utc("2024-01-01T00:00:00Z"), utc("2024-01-01T03:00:00Z"))
	eq(t, got, "2024-01-01T01:00:00Z", "2024-01-01T02:00:00Z", "2024-01-01T03:00:00Z")

	// endDateTime bounds the series.
	got, _ = c.Occurrences(utc("2024-01-01T22:00:00Z"), utc("2024-01-05T00:00:00Z"))
	eq(t, got, "2024-01-01T23:00:00Z", "2024-01-02T00:00:00Z")

	// Nothing before the start.
	if got, _ := c.Occurrences(utc("2023-01-01T00:00:00Z"), utc("2023-06-01T00:00:00Z")); len(got) != 0 {
		t.Fatalf("fired before start: %v", fired(got))
	}
}

func TestCronCatchUpIsCappedAndKeepsTheNewest(t *testing.T) {
	// One minute apart over a year: advancing the clock that far must not
	// materialise half a million job instances.
	c := mustParse(t, `{"type":"Cron","interval":1,"startDateTime":"2024-01-01T00:00:00Z",
		"endDateTime":"2025-01-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, trunc := c.Occurrences(c.Start().Add(-time.Second), utc("2024-12-31T00:00:00Z"))
	if !trunc {
		t.Fatal("not reported as truncated")
	}
	if len(got) != MaxOccurrencesPerTick {
		t.Fatalf("kept %d, want the %d cap", len(got), MaxOccurrencesPerTick)
	}
	// The newest are the ones kept — a missed window is dropped, not backfilled.
	if last := got[len(got)-1].UTC().Format(time.RFC3339); last != "2024-12-31T00:00:00Z" {
		t.Fatalf("last kept = %s, want the most recent occurrence", last)
	}
}

// ---- Daily / Weekly ----

func TestDailyOccurrencesSortAndDedupeTimes(t *testing.T) {
	c := mustParse(t, `{"type":"Daily","times":["18:30","06:00","06:00"],
		"startDateTime":"2024-03-01T00:00:00Z","endDateTime":"2024-03-04T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-03-03T00:00:00Z"))
	eq(t, got,
		"2024-03-01T06:00:00Z", "2024-03-01T18:30:00Z",
		"2024-03-02T06:00:00Z", "2024-03-02T18:30:00Z")
}

func TestDailyDoesNotFireTimesBeforeTheStartInstant(t *testing.T) {
	// Start at noon: that day's 06:00 has already gone by and must not fire.
	c := mustParse(t, `{"type":"Daily","times":["06:00","18:00"],
		"startDateTime":"2024-03-01T12:00:00Z","endDateTime":"2024-03-03T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-03-02T23:00:00Z"))
	eq(t, got, "2024-03-01T18:00:00Z", "2024-03-02T06:00:00Z", "2024-03-02T18:00:00Z")
}

func TestWeeklyFiresOnlyOnItsWeekdays(t *testing.T) {
	// 2024-04-01 is a Monday.
	c := mustParse(t, `{"type":"Weekly","times":["09:00"],"weekdays":["Monday","Friday"],
		"startDateTime":"2024-04-01T00:00:00Z","endDateTime":"2024-05-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-04-09T00:00:00Z"))
	eq(t, got, "2024-04-01T09:00:00Z", "2024-04-05T09:00:00Z", "2024-04-08T09:00:00Z")
}

// ---- Monthly ----

func TestMonthlyDayOfMonthSkipsMonthsWithoutThatDay(t *testing.T) {
	// "The 31st" does not slide to the 28th — February simply has no occurrence.
	c := mustParse(t, `{"type":"Monthly","times":["00:00"],"recurrence":1,
		"occurrence":{"type":"DayOfMonth","dayOfMonth":31},
		"startDateTime":"2024-01-01T00:00:00Z","endDateTime":"2024-06-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-06-01T00:00:00Z"))
	eq(t, got, "2024-01-31T00:00:00Z", "2024-03-31T00:00:00Z", "2024-05-31T00:00:00Z")
}

func TestMonthlyRecurrenceCountsFromTheStartMonth(t *testing.T) {
	c := mustParse(t, `{"type":"Monthly","times":["12:00"],"recurrence":3,
		"occurrence":{"type":"DayOfMonth","dayOfMonth":15},
		"startDateTime":"2024-02-01T00:00:00Z","endDateTime":"2025-02-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-12-01T00:00:00Z"))
	eq(t, got, "2024-02-15T12:00:00Z", "2024-05-15T12:00:00Z", "2024-08-15T12:00:00Z", "2024-11-15T12:00:00Z")
}

func TestMonthlyOrdinalWeekday(t *testing.T) {
	// Third Tuesday: 2024-01-16, 2024-02-20, 2024-03-19.
	c := mustParse(t, `{"type":"Monthly","times":["08:00"],"recurrence":1,
		"occurrence":{"type":"OrdinalWeekday","weekIndex":"Third","weekday":"Tuesday"},
		"startDateTime":"2024-01-01T00:00:00Z","endDateTime":"2024-04-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-04-01T00:00:00Z"))
	eq(t, got, "2024-01-16T08:00:00Z", "2024-02-20T08:00:00Z", "2024-03-19T08:00:00Z")
}

func TestMonthlyFifthWeekdaySkipsMonthsThatLackOne(t *testing.T) {
	// A fifth Wednesday exists in Jan 2025 (29th) and Apr 2025 (30th) but not
	// in Feb or Mar 2025.
	c := mustParse(t, `{"type":"Monthly","times":["00:00"],"recurrence":1,
		"occurrence":{"type":"OrdinalWeekday","weekIndex":"Fifth","weekday":"Wednesday"},
		"startDateTime":"2025-01-01T00:00:00Z","endDateTime":"2025-05-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2025-05-01T00:00:00Z"))
	eq(t, got, "2025-01-29T00:00:00Z", "2025-04-30T00:00:00Z")
}

// ---- time zones ----

func TestDailyHoldsLocalWallTimeAcrossDST(t *testing.T) {
	// US DST began 2024-03-10. 09:00 New York is 14:00Z before and 13:00Z
	// after — a schedule that stored a fixed UTC offset would drift an hour.
	c := mustParse(t, `{"type":"Daily","times":["09:00"],
		"startDateTime":"2024-03-08T00:00:00","endDateTime":"2024-03-13T00:00:00",
		"localTimeZoneId":"Eastern Standard Time"}`)
	got, _ := c.Occurrences(c.Start().Add(-time.Second), utc("2024-03-12T23:00:00Z"))
	eq(t, got,
		"2024-03-08T14:00:00Z", "2024-03-09T14:00:00Z",
		"2024-03-10T13:00:00Z", "2024-03-11T13:00:00Z", "2024-03-12T13:00:00Z")
}

func TestCalendarCatchUpIsCappedToo(t *testing.T) {
	c := mustParse(t, `{"type":"Daily","times":["00:00","06:00","12:00","18:00"],
		"startDateTime":"2020-01-01T00:00:00Z","endDateTime":"2030-01-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	got, trunc := c.Occurrences(c.Start().Add(-time.Second), utc("2024-01-01T00:00:00Z"))
	if !trunc || len(got) != MaxOccurrencesPerTick {
		t.Fatalf("len=%d truncated=%v, want %d and true", len(got), trunc, MaxOccurrencesPerTick)
	}
	if last := got[len(got)-1].UTC().Format(time.RFC3339); last != "2024-01-01T00:00:00Z" {
		t.Fatalf("last = %s, want the newest occurrence", last)
	}
}

func TestEmptyWindows(t *testing.T) {
	c := mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00Z",
		"endDateTime":"2024-01-02T00:00:00Z","localTimeZoneId":"UTC"}`)
	// until <= after
	if got, _ := c.Occurrences(utc("2024-01-01T05:00:00Z"), utc("2024-01-01T05:00:00Z")); got != nil {
		t.Fatalf("degenerate window fired %v", fired(got))
	}
	// entirely past endDateTime
	if got, _ := c.Occurrences(utc("2024-02-01T00:00:00Z"), utc("2024-03-01T00:00:00Z")); got != nil {
		t.Fatalf("fired past end: %v", fired(got))
	}
	// mid-interval window with no occurrence in it
	if got, _ := c.Occurrences(utc("2024-01-01T01:10:00Z"), utc("2024-01-01T01:50:00Z")); got != nil {
		t.Fatalf("fired between ticks: %v", fired(got))
	}
	// a calendar schedule whose window falls between its times
	d := mustParse(t, `{"type":"Daily","times":["09:00"],"startDateTime":"2024-01-01T00:00:00Z",
		"endDateTime":"2024-02-01T00:00:00Z","localTimeZoneId":"UTC"}`)
	if got, _ := d.Occurrences(utc("2024-01-01T10:00:00Z"), utc("2024-01-01T11:00:00Z")); got != nil {
		t.Fatalf("daily fired between times: %v", fired(got))
	}
}

func TestEndAccessor(t *testing.T) {
	c := mustParse(t, `{"type":"Cron","interval":60,"startDateTime":"2024-01-01T00:00:00Z",
		"endDateTime":"2024-01-02T00:00:00Z","localTimeZoneId":"UTC"}`)
	if got := c.End().UTC().Format(time.RFC3339); got != "2024-01-02T00:00:00Z" {
		t.Fatalf("End() = %s", got)
	}
}
