package testsupport

import "time"

// UTCDay is the current UTC calendar day, spelled the way the Power BI
// activity-log API spells its bounds.
//
// A test that needs a day-scoped window must capture it ONCE, BEFORE the
// operations whose events the window has to contain, and must widen with
// UTCDaysSince at the moment it queries. Reading the day AFTER the operations
// is the defect this pair exists to remove: the events are stamped from the
// store's clock and the window from a second, independent read of the wall
// clock, so a run that crosses UTC midnight between the two reads queries a day
// the events are not in. It fires in a sub-second window once per day, and
// neither `-race` nor `-shuffle=on` can see it -- there is no shared memory and
// no cross-test ordering involved, only two readings of one clock.
func UTCDay() string {
	return time.Now().UTC().Format("2006-01-02")
}

// UTCDaysSince returns captured, plus the current UTC day when the day has
// advanced since captured was read.
//
// WHY THE STRADDLE IS UNIONED RATHER THAN THE WINDOW WIDENED. The API refuses a
// window spanning two days outright -- internal/server/activityevents_test.go's
// "spans two days" case pins that refusal at 400 -- so "ask for a two-day
// window" is not an available repair. Two single-day queries are, and they are
// the more honest shape anyway: the straddle is detected and handled rather
// than hoped against, and a reader can see exactly which days were asked for.
//
// Both orderings are covered, which is the half worth stating. Events stamped
// just before midnight with the query just after are found in captured; events
// stamped just after midnight with the capture just before are found in the
// current day. Either way the union holds them.
func UTCDaysSince(captured string) []string {
	if now := UTCDay(); now != captured {
		return []string{captured, now}
	}
	return []string{captured}
}
