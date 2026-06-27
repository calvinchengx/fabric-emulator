package pipeline

import (
	"strings"
	"testing"
	"time"
)

// TestExpressionUtcNowFollowsInjectedClock covers Fabric's date functions.
//
// The point of the injected clock is that @utcNow() is the one expression
// whose answer is not a function of the definition: it has to come from the
// EMULATOR's clock, the same one /_emulator/clock freezes and advances and
// every other dated artefact is stamped from. A run under a frozen clock must
// therefore be byte-for-byte reproducible, which is what the exact-string
// assertions below pin down — including the seven fractional digits of the
// round-trip format, which a plain RFC3339 formatter drops when they are zero.
func TestExpressionUtcNowFollowsInjectedClock(t *testing.T) {
	frozen := time.Date(2024, 3, 13, 19, 39, 35, 280_000_000, time.UTC)

	def := `{"properties":{
      "variables":{"now":{"type":"String"},"roundTrip":{"type":"String"},
                   "yesterday":{"type":"String"},"plus5h":{"type":"String"},
                   "minus90m":{"type":"String"},"plus30s":{"type":"String"},
                   "stamped":{"type":"String"}},
      "activities":[
        {"name":"now","type":"SetVariable","typeProperties":{"variableName":"now","value":"@utcNow()"}},
        {"name":"roundTrip","type":"SetVariable","typeProperties":{"variableName":"roundTrip","value":"@utcNow('o')"}},
        {"name":"yesterday","type":"SetVariable","typeProperties":{"variableName":"yesterday","value":"@addDays(utcNow(), -1)"}},
        {"name":"plus5h","type":"SetVariable","typeProperties":{"variableName":"plus5h","value":"@addHours('2024-03-13T19:39:35.0000000Z', 5)"}},
        {"name":"minus90m","type":"SetVariable","typeProperties":{"variableName":"minus90m","value":"@addMinutes(utcNow(), -90)"}},
        {"name":"plus30s","type":"SetVariable","typeProperties":{"variableName":"plus30s","value":"@addSeconds('2024-03-13T19:39:35Z', 30)"}},
        {"name":"stamped","type":"SetVariable","typeProperties":{"variableName":"stamped","value":"Files/landing/@{utcNow()}/orders.csv"}}
      ]}}`

	p, err := Parse([]byte(def))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := p.RunWith(nil, nil, Options{Now: func() time.Time { return frozen }})
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	for _, c := range []struct{ name, want string }{
		{"now", "2024-03-13T19:39:35.2800000Z"},
		{"roundTrip", "2024-03-13T19:39:35.2800000Z"},
		// Crossing a day boundary backwards and forwards.
		{"yesterday", "2024-03-12T19:39:35.2800000Z"},
		{"plus5h", "2024-03-14T00:39:35.0000000Z"},
		{"minus90m", "2024-03-13T18:09:35.2800000Z"},
		// A timestamp with no fractional digits still comes back with seven.
		{"plus30s", "2024-03-13T19:40:05.0000000Z"},
		// And the functions interpolate into literal text, which is how a
		// definition actually stamps a landing folder.
		{"stamped", "Files/landing/2024-03-13T19:39:35.2800000Z/orders.csv"},
	} {
		if got := res.Variables[c.name]; got != c.want {
			t.Errorf("%s = %v (%T), want %q", c.name, got, got, c.want)
		}
	}

	// Moving the clock moves utcNow(): the value is read per call, not captured
	// once. This is what makes an advanced /_emulator/clock visible to a run.
	later := frozen.Add(36 * time.Hour)
	res2 := p.RunWith(nil, nil, Options{Now: func() time.Time { return later }})
	if res2.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res2.Status, res2.Error)
	}
	if got, want := res2.Variables["now"], "2024-03-15T07:39:35.2800000Z"; got != want {
		t.Errorf("after advancing the clock, now = %v, want %q", got, want)
	}
	if res2.Variables["yesterday"] != "2024-03-14T07:39:35.2800000Z" {
		t.Errorf("yesterday = %v", res2.Variables["yesterday"])
	}

	// A non-UTC injected clock is still reported in UTC — the round-trip format
	// ends in 'Z' and would otherwise be a lie.
	east := time.FixedZone("UTC+8", 8*3600)
	res3 := p.RunWith(nil, nil, Options{Now: func() time.Time { return frozen.In(east) }})
	if got := res3.Variables["now"]; got != "2024-03-13T19:39:35.2800000Z" {
		t.Errorf("non-UTC clock: now = %v", got)
	}

	// --- direct expression checks --------------------------------------------

	ctx := &evalContext{Now: func() time.Time { return frozen }}
	for _, c := range []struct{ expr, want string }{
		{"@addSeconds(utcNow(), 0)", "2024-03-13T19:39:35.2800000Z"},
		{"@addMinutes('2024-03-13T19:39:35.28Z', 1)", "2024-03-13T19:40:35.2800000Z"},
		// A timestamp with an offset is converted to UTC, not carried along.
		{"@addHours('2024-03-13T19:39:35+02:00', 0)", "2024-03-13T17:39:35.0000000Z"},
		// A bare ISO 8601 timestamp (no designator) is read as UTC.
		{"@addDays('2024-03-13T19:39:35', 1)", "2024-03-14T19:39:35.0000000Z"},
		{"@addDays('2024-02-28', 2)", "2024-03-01T00:00:00.0000000Z"}, // leap year
		// The explicit format argument is accepted on the add* functions too.
		{"@addHours('2024-03-13T19:39:35Z', -19, 'o')", "2024-03-13T00:39:35.0000000Z"},
		// Composition: the output of one is a valid input to the next.
		{"@addDays(addHours(utcNow(), 24), -1)", "2024-03-13T19:39:35.2800000Z"},
		{"@substring(utcNow(), 0, 10)", "2024-03-13"},
	} {
		got, err := evalString(c.expr, ctx)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %v (%T), want %q", c.expr, got, got, c.want)
		}
	}

	// A format string other than the round-trip 'o' is an explicit error. Half
	// a format vocabulary would answer some definitions with a silent default,
	// and a pipeline naming a file @utcNow('yyyyMMdd') would then write the
	// wrong name on every run without ever failing.
	for _, expr := range []string{
		"@utcNow('dd/MM/yyyy')",
		"@utcNow('yyyyMMdd')",
		"@utcNow('D')",
		"@utcNow('')",
		"@utcNow(1)",
		"@utcNow('o', 'o')", // too many arguments
		"@addDays('2024-03-13T19:39:35Z', 1, 'D')",   // same rule for add*
		"@addDays('13/03/2024', 1)",                  // not an ISO 8601 timestamp
		"@addDays('not-a-timestamp', 1)",             //
		"@addHours('2024-03-13T19:39:35Z', 'one')",   // the interval must be a number
		"@addMinutes('2024-03-13T19:39:35Z', null)",  // (null is not "add zero")
		"@addSeconds('2024-03-13T19:39:35Z', 1.5)",   // nor a fraction of a unit
		"@addDays(utcNow())",                         // missing the interval
		"@addDays()",                                 //
		"@addHours('2024-03-13T19:39:35Z',1,'o',2)",  // too many
		"@addSeconds(1710358775, 1)",                 // an epoch number is not a timestamp
		"@addDays(createArray('2024-03-13'), 1)",     //
		"@addMinutes('2024-03-13T19:39:35Z', '1.5')", // a numeric string still has to be whole
	} {
		if got, err := evalString(expr, ctx); err == nil {
			t.Errorf("%s: expected an error, got %#v", expr, got)
		}
	}

	// A bad format inside a run fails the activity rather than substituting a
	// default — the same way an unsupported function does.
	badDef := `{"properties":{"variables":{"v":{"type":"String"}},"activities":[
        {"name":"stamp","type":"SetVariable","typeProperties":{"variableName":"v","value":"@utcNow('dd/MM/yyyy')"}}
      ]}}`
	bad, err := Parse([]byte(badDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	badRes := bad.RunWith(nil, nil, Options{Now: func() time.Time { return frozen }})
	if badRes.Status != StatusFailed {
		t.Errorf("an unsupported format should fail the activity, got %s", badRes.Status)
	}

	// With no clock wired (a caller that never set Options.Now), the functions
	// still work off the wall clock instead of returning the zero time.
	got, err := evalString("@utcNow()", &evalContext{})
	if err != nil {
		t.Fatalf("utcNow with no injected clock: %v", err)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("utcNow returned %T, want string", got)
	}
	if !strings.HasSuffix(s, "Z") || len(s) != len("2024-03-13T19:39:35.2800000Z") {
		t.Errorf("utcNow() = %q, want the round-trip format", s)
	}
	wall, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("utcNow() = %q: %v", s, err)
	}
	if d := time.Since(wall); d < -time.Minute || d > time.Minute {
		t.Errorf("utcNow() = %q, which is %v from now", s, d)
	}
}
