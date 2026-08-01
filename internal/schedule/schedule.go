// Package schedule implements Fabric's item-job ScheduleConfig — the
// discriminated union (Cron / Daily / Weekly / Monthly) carried by the Job
// Scheduler API — and the occurrence arithmetic that turns one into a series
// of fire times.
//
// It is deliberately a pure package: no HTTP, no store, no wall clock. The
// caller supplies the window, so the emulator's controllable clock drives
// firing deterministically (advance an hour, get the hour's occurrences) and
// every rule here is unit-testable without a server.
//
// # Time zones
//
// `localTimeZoneId` is a **Windows** time-zone id in real Fabric
// ("Pacific Standard Time"), not an IANA one ("America/Los_Angeles"). Go only
// knows IANA, so windowsZones below maps the ids fabric-docs samples actually
// use, and IANA names are accepted directly as well — the emulator is likelier
// to be driven from a Linux/macOS client than from a Windows one. An
// unrecognised id is a validation error rather than a silent fallback to UTC:
// a schedule that fires at the wrong hour is worse than one that refuses to
// be created.
package schedule

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	// The emulator ships as a distroless image with no /usr/share/zoneinfo, so
	// the zone database must be embedded or every localTimeZoneId but UTC
	// would fail to load in the container while passing on a dev machine.
	_ "time/tzdata"
)

// Config types (the `type` discriminator of ScheduleConfig).
const (
	TypeCron    = "Cron"
	TypeDaily   = "Daily"
	TypeWeekly  = "Weekly"
	TypeMonthly = "Monthly"
)

// Monthly occurrence types.
const (
	OccurrenceDayOfMonth     = "DayOfMonth"
	OccurrenceOrdinalWeekday = "OrdinalWeekday"
)

// Documented bounds. Cron's interval ceiling (5,270,400 minutes ≈ 10 years)
// and the 100-entry `times` cap come from the ScheduleConfig reference.
const (
	MinInterval   = 1
	MaxInterval   = 5270400
	MaxTimes      = 100
	MaxWeekdays   = 7
	MinRecurrence = 1
	MaxRecurrence = 12
)

// MaxOccurrencesPerTick bounds how many job instances one schedule may
// materialise in a single evaluation.
//
// This is an emulator boundary with a reason: the clock is controllable, so a
// caller can advance a year against a one-minute Cron and ask for half a
// million job instances. Real Fabric never faces that because its clock only
// moves forward at one second per second. When more than this many
// occurrences are due, the **most recent** ones win and the earlier ones are
// dropped — the same "no backfill storm" choice ordinary schedulers make for
// missed windows.
const MaxOccurrencesPerTick = 100

// maxScanDays bounds the calendar walk for Daily/Weekly/Monthly so a pathological
// window cannot spin. 4000 days ≈ 11 years, comfortably past the widest
// monthly recurrence (12 months).
const maxScanDays = 4000

// Occurrence is a Monthly schedule's day selector: either a day number, or an
// ordinal weekday ("the third Tuesday").
type Occurrence struct {
	Type       string `json:"type"`
	DayOfMonth int    `json:"dayOfMonth,omitempty"`
	WeekIndex  string `json:"weekIndex,omitempty"`
	Weekday    string `json:"weekday,omitempty"`
}

// Config is a parsed, validated ScheduleConfig.
type Config struct {
	Type            string      `json:"type"`
	StartDateTime   string      `json:"startDateTime"`
	EndDateTime     string      `json:"endDateTime"`
	LocalTimeZoneID string      `json:"localTimeZoneId"`
	Interval        int         `json:"interval,omitempty"`
	Times           []string    `json:"times,omitempty"`
	Weekdays        []string    `json:"weekdays,omitempty"`
	Occurrence      *Occurrence `json:"occurrence,omitempty"`
	Recurrence      int         `json:"recurrence,omitempty"`

	// Resolved during Parse so occurrence arithmetic needn't re-parse.
	loc     *time.Location
	start   time.Time
	end     time.Time
	minutes []int // times[] as minutes-since-midnight, sorted, deduped
	days    map[time.Weekday]bool
}

// Error is a validation failure carrying the Fabric error code the API should
// return, so the wire code is decided by the rule that failed rather than
// flattened to one generic code at the boundary.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(format string, args ...any) *Error {
	return &Error{Code: "InvalidRequest", Message: fmt.Sprintf(format, args...)}
}

// windowsZones maps the Windows time-zone ids fabric-docs uses to IANA names.
// Not the full CLDR table (≈140 entries): the ones a Fabric sample or a
// plausible test actually names, plus UTC. Extending it is a one-line change;
// pretending to support an id we cannot resolve is not.
var windowsZones = map[string]string{
	"utc":                            "UTC",
	"gmt standard time":              "Europe/London",
	"greenwich standard time":        "Atlantic/Reykjavik",
	"w. europe standard time":        "Europe/Berlin",
	"central europe standard time":   "Europe/Budapest",
	"central european standard time": "Europe/Warsaw",
	"romance standard time":          "Europe/Paris",
	"e. europe standard time":        "Europe/Chisinau",
	"russian standard time":          "Europe/Moscow",
	"eastern standard time":          "America/New_York",
	"central standard time":          "America/Chicago",
	"mountain standard time":         "America/Denver",
	"pacific standard time":          "America/Los_Angeles",
	"us eastern standard time":       "America/Indiana/Indianapolis",
	"alaskan standard time":          "America/Anchorage",
	"hawaiian standard time":         "Pacific/Honolulu",
	"tokyo standard time":            "Asia/Tokyo",
	"china standard time":            "Asia/Shanghai",
	"singapore standard time":        "Asia/Singapore",
	"india standard time":            "Asia/Kolkata",
	"korea standard time":            "Asia/Seoul",
	"aus eastern standard time":      "Australia/Sydney",
	"w. australia standard time":     "Australia/Perth",
	"new zealand standard time":      "Pacific/Auckland",
	"e. south america standard time": "America/Sao_Paulo",
	"south africa standard time":     "Africa/Johannesburg",
	"arabian standard time":          "Asia/Dubai",
}

// LoadLocation resolves a Fabric localTimeZoneId: a Windows id from the table
// above, or an IANA name passed straight through to the embedded tzdata.
func LoadLocation(id string) (*time.Location, error) {
	if id == "" {
		return nil, invalid("localTimeZoneId is required.")
	}
	if iana, ok := windowsZones[strings.ToLower(strings.TrimSpace(id))]; ok {
		id = iana
	}
	loc, err := time.LoadLocation(id)
	if err != nil {
		return nil, invalid("localTimeZoneId %q is not a time zone this emulator knows "+
			"(Windows ids from the fabric-docs samples, or any IANA name).", id)
	}
	return loc, nil
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday,
	"friday": time.Friday, "saturday": time.Saturday,
}

var weekIndexes = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5}

// Parse validates raw ScheduleConfig JSON into a Config. Every documented
// bound is enforced here so an invalid schedule is refused at create time
// rather than silently never firing.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("configuration is not valid JSON: %v", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch c.Type {
	case TypeCron, TypeDaily, TypeWeekly, TypeMonthly:
	case "":
		return invalid("configuration.type is required.")
	default:
		return invalid("configuration.type %q is not one of Cron, Daily, Weekly, Monthly.", c.Type)
	}
	loc, err := LoadLocation(c.LocalTimeZoneID)
	if err != nil {
		return err
	}
	c.loc = loc
	if c.start, err = parseLocalTime(c.StartDateTime, loc, "startDateTime"); err != nil {
		return err
	}
	if c.end, err = parseLocalTime(c.EndDateTime, loc, "endDateTime"); err != nil {
		return err
	}
	if !c.end.After(c.start) {
		return invalid("endDateTime must be after startDateTime.")
	}

	switch c.Type {
	case TypeCron:
		if c.Interval < MinInterval || c.Interval > MaxInterval {
			return invalid("configuration.interval must be between %d and %d minutes.", MinInterval, MaxInterval)
		}
		return c.rejectUnused("times", len(c.Times) > 0, "weekdays", len(c.Weekdays) > 0)
	case TypeDaily:
		if err := c.parseTimes(); err != nil {
			return err
		}
		return c.rejectUnused("weekdays", len(c.Weekdays) > 0, "occurrence", c.Occurrence != nil)
	case TypeWeekly:
		if err := c.parseTimes(); err != nil {
			return err
		}
		if err := c.parseWeekdays(); err != nil {
			return err
		}
		return c.rejectUnused("occurrence", c.Occurrence != nil)
	default: // Monthly
		if err := c.parseTimes(); err != nil {
			return err
		}
		if c.Recurrence < MinRecurrence || c.Recurrence > MaxRecurrence {
			return invalid("configuration.recurrence must be between %d and %d months.", MinRecurrence, MaxRecurrence)
		}
		return c.parseOccurrence()
	}
}

// rejectUnused refuses fields that belong to a *different* member of the
// union. Silently ignoring them is how a caller ends up with a Cron schedule
// that they believe honours `times` — the wrong-shaped request should fail.
func (c *Config) rejectUnused(pairs ...any) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if present, _ := pairs[i+1].(bool); present {
			return invalid("configuration.%v does not apply to a %s schedule.", pairs[i], c.Type)
		}
	}
	return nil
}

func (c *Config) parseTimes() error {
	if len(c.Times) == 0 {
		return invalid("configuration.times is required for a %s schedule.", c.Type)
	}
	if len(c.Times) > MaxTimes {
		return invalid("configuration.times accepts at most %d entries.", MaxTimes)
	}
	seen := map[int]bool{}
	for _, t := range c.Times {
		m, err := parseHHMM(t)
		if err != nil {
			return err
		}
		if !seen[m] {
			seen[m] = true
			c.minutes = append(c.minutes, m)
		}
	}
	sort.Ints(c.minutes)
	return nil
}

func (c *Config) parseWeekdays() error {
	if len(c.Weekdays) == 0 {
		return invalid("configuration.weekdays is required for a Weekly schedule.")
	}
	if len(c.Weekdays) > MaxWeekdays {
		return invalid("configuration.weekdays accepts at most %d entries.", MaxWeekdays)
	}
	c.days = map[time.Weekday]bool{}
	for _, d := range c.Weekdays {
		wd, ok := weekdayNames[strings.ToLower(strings.TrimSpace(d))]
		if !ok {
			return invalid("configuration.weekdays entry %q is not a weekday name (Monday…Sunday).", d)
		}
		c.days[wd] = true
	}
	return nil
}

func (c *Config) parseOccurrence() error {
	if c.Occurrence == nil {
		return invalid("configuration.occurrence is required for a Monthly schedule.")
	}
	switch c.Occurrence.Type {
	case OccurrenceDayOfMonth:
		if c.Occurrence.DayOfMonth < 1 || c.Occurrence.DayOfMonth > 31 {
			return invalid("configuration.occurrence.dayOfMonth must be between 1 and 31.")
		}
	case OccurrenceOrdinalWeekday:
		if _, ok := weekIndexes[strings.ToLower(c.Occurrence.WeekIndex)]; !ok {
			return invalid("configuration.occurrence.weekIndex %q is not one of First…Fifth.", c.Occurrence.WeekIndex)
		}
		if _, ok := weekdayNames[strings.ToLower(c.Occurrence.Weekday)]; !ok {
			return invalid("configuration.occurrence.weekday %q is not a weekday name.", c.Occurrence.Weekday)
		}
	case "":
		return invalid("configuration.occurrence.type is required.")
	default:
		return invalid("configuration.occurrence.type %q is not one of DayOfMonth, OrdinalWeekday.", c.Occurrence.Type)
	}
	return nil
}

// parseLocalTime reads a Fabric schedule date-time. The reference writes them
// without an offset ("2024-04-28T00:00:00") because they are *local to
// localTimeZoneId*; a trailing Z is accepted too and read as UTC, since that
// is what a client formatting with RFC3339 will send.
func parseLocalTime(s string, loc *time.Location, field string) (time.Time, error) {
	if s == "" {
		return time.Time{}, invalid("configuration.%s is required.", field)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil // carries its own offset (…Z or …+05:30)
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, invalid("configuration.%s %q is not an ISO-8601 date-time.", field, s)
}

func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, invalid("configuration.times entry %q is not hh:mm.", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, invalid("configuration.times entry %q is not a valid hh:mm time of day.", s)
	}
	return hh*60 + mm, nil
}

// Start and End expose the resolved window (both instants, zone applied).
func (c *Config) Start() time.Time { return c.start }
func (c *Config) End() time.Time   { return c.end }

// Occurrences returns the schedule's fire times in the half-open window
// (after, until], oldest first, capped at MaxOccurrencesPerTick.
//
// The window is half-open at the lower bound so a caller can pass "the last
// time I fired" and never fire the same instant twice. When more occurrences
// are due than the cap allows, the newest are kept and truncated is true —
// see MaxOccurrencesPerTick for why.
func (c *Config) Occurrences(after, until time.Time) (times []time.Time, truncated bool) {
	// Nothing before the schedule starts, nothing after it ends.
	if lower := c.start.Add(-time.Nanosecond); after.Before(lower) {
		after = lower
	}
	if until.After(c.end) {
		until = c.end
	}
	if !until.After(after) {
		return nil, false
	}
	if c.Type == TypeCron {
		return c.cronOccurrences(after, until)
	}
	return c.calendarOccurrences(after, until)
}

// cronOccurrences is closed-form: occurrences are start + k·interval, so the
// bounding k values are computed rather than walked. That keeps a one-minute
// interval over a decade-wide window O(cap) instead of O(minutes).
func (c *Config) cronOccurrences(after, until time.Time) ([]time.Time, bool) {
	step := time.Duration(c.Interval) * time.Minute
	// First k with start + k·step > after.
	first := int64(0)
	if d := after.Sub(c.start); d >= 0 {
		first = int64(d/step) + 1
	}
	last := int64(until.Sub(c.start) / step)
	if last < first {
		return nil, false
	}
	truncated := false
	if n := last - first + 1; n > MaxOccurrencesPerTick {
		first = last - MaxOccurrencesPerTick + 1
		truncated = true
	}
	out := make([]time.Time, 0, last-first+1)
	for k := first; k <= last; k++ {
		out = append(out, c.start.Add(time.Duration(k)*step))
	}
	return out, truncated
}

// calendarOccurrences walks days in the schedule's own zone — the only way to
// get Daily/Weekly/Monthly right across a DST boundary, where "09:00 local"
// is not a fixed number of hours after the previous 09:00.
func (c *Config) calendarOccurrences(after, until time.Time) ([]time.Time, bool) {
	var out []time.Time
	truncated := false
	day := after.In(c.loc)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, c.loc)
	endDay := until.In(c.loc)
	for scanned := 0; !day.After(endDay) && scanned < maxScanDays; scanned, day = scanned+1, day.AddDate(0, 0, 1) {
		if !c.dayQualifies(day) {
			continue
		}
		for _, m := range c.minutes {
			t := time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, c.loc)
			if !t.After(after) || t.After(until) {
				continue
			}
			out = append(out, t)
			if len(out) > MaxOccurrencesPerTick {
				out = out[1:] // keep the newest
				truncated = true
			}
		}
	}
	return out, truncated
}

// dayQualifies decides whether a calendar day is one this schedule fires on.
func (c *Config) dayQualifies(day time.Time) bool {
	switch c.Type {
	case TypeDaily:
		return true
	case TypeWeekly:
		return c.days[day.Weekday()]
	}
	// Monthly: the month must be on the recurrence cadence counted from the
	// start month, and the day must be the selected one.
	start := c.start.In(c.loc)
	months := (day.Year()-start.Year())*12 + int(day.Month()) - int(start.Month())
	if months < 0 || months%c.Recurrence != 0 {
		return false
	}
	if c.Occurrence.Type == OccurrenceDayOfMonth {
		// A day past the month's length simply does not occur that month —
		// "the 31st" skips February rather than sliding to the 28th.
		return day.Day() == c.Occurrence.DayOfMonth
	}
	wd := weekdayNames[strings.ToLower(c.Occurrence.Weekday)]
	if day.Weekday() != wd {
		return false
	}
	// Which occurrence of this weekday within the month this day is.
	return (day.Day()-1)/7+1 == weekIndexes[strings.ToLower(c.Occurrence.WeekIndex)]
}
