package pipeline

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Fabric's date functions. Their answers depend on the CLOCK, which is the
// only thing in the expression library that does — so the clock is injected
// (Options.Now -> run.now -> evalContext.Now) rather than read from the
// wall. The emulator's own clock can be frozen and advanced through
// /_emulator/clock, and a pipeline that stamps a landing folder with
// `@utcNow()` has to move with it; reading time.Now here would make that one
// expression the single thing in the emulator that ignores the clock.

// isoRoundTrip is Fabric's 'o' format specifier —
// `yyyy-MM-ddTHH:mm:ss.fffffffZ`, the .NET round-trip form: exactly seven
// fractional digits, always present, always UTC. It is what utcNow() returns
// with no arguments, and the format every add* function returns.
const isoRoundTrip = "2006-01-02T15:04:05.0000000Z"

// now is the run's clock, in UTC. A nil Now (a context built by hand, or a
// caller that never wired one) falls back to the wall clock.
func (c *evalContext) now() time.Time {
	if c == nil || c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// roundTripFormat accepts only Fabric's 'o' specifier.
//
// The .NET format vocabulary these functions take ('D', 'dd/MM/yyyy', …) is a
// language of its own, and half-implementing it would answer some format
// strings correctly and others with a silent default — a pipeline that names a
// file `@utcNow('yyyyMMdd')` would then write one file per run under the wrong
// name and never fail. Refusing what is not implemented keeps the subset
// honest, the same way an unknown function does.
func roundTripFormat(name string, v value) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%s expects a format string, got %T", name, v)
	}
	if s != "o" && s != "O" {
		return fmt.Errorf("%s supports only the round-trip format 'o' "+
			"(yyyy-MM-ddTHH:mm:ss.fffffffZ), got %q", name, s)
	}
	return nil
}

// shiftTimestamp is addDays/addHours/addMinutes/addSeconds: parse an ISO 8601
// timestamp, shift it by a whole number of units (negative shifts back), and
// return it in the round-trip format.
func shiftTimestamp(name string, args []value) (value, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, fmt.Errorf("%s expects 2 or 3 argument(s), got %d", name, len(args))
	}
	if len(args) == 3 {
		if err := roundTripFormat(name, args[2]); err != nil {
			return nil, err
		}
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("%s expects a timestamp string, got %T", name, args[0])
	}
	ts, err := parseTimestamp(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	n, err := wholeNumber(name, args[1])
	if err != nil {
		return nil, err
	}
	switch name {
	case "addDays":
		// Calendar days, not 24h multiples. Identical in UTC, but AddDate is
		// what the function means and survives a future local-zone timestamp.
		ts = ts.AddDate(0, 0, n)
	case "addHours":
		ts = ts.Add(time.Duration(n) * time.Hour)
	case "addMinutes":
		ts = ts.Add(time.Duration(n) * time.Minute)
	case "addSeconds":
		ts = ts.Add(time.Duration(n) * time.Second)
	default:
		return nil, fmt.Errorf("unsupported function %q", name)
	}
	return ts.Format(isoRoundTrip), nil
}

// parseTimestamp reads an ISO 8601 timestamp. A value without a zone
// designator is read as UTC — Fabric's date functions are UTC throughout — and
// one with an offset is converted, so the result is always comparable.
func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339,          // with 'Z' or an offset; a fraction is optional
		"2006-01-02T15:04:05", // bare ISO 8601, read as UTC
		"2006-01-02 15:04:05", // the space-separated spelling SQL emits
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as an ISO 8601 timestamp "+
		"(expected yyyy-MM-ddTHH:mm:ss[.fffffff][Z])", s)
}

// wholeNumber is the shift argument. toNumber() would answer 0 for a typo like
// 'one' or a null, and "add zero days" is a wrong answer that looks like a
// right one; an interval has to be stated, so anything that is not a whole
// number is an error.
func wholeNumber(name string, v value) (int, error) {
	switch t := v.(type) {
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("%s expects a whole number of units, got %q", name, t)
		}
		return n, nil
	case float64, float32, int, int8, int16, int32, int64:
		n := toNumber(v)
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("%s expects a whole number of units, got %v", name, n)
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("%s expects a whole number of units, got %T", name, v)
}
