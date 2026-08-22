// Package cron parses five-field cron expressions and answers when they next
// fire. It owns no goroutine and performs no I/O: callers drive the clock.
//
// The dialect is Vixie cron, restricted to what a process manager needs. There
// is no seconds field: a schedule that needs sub-minute precision is not a
// schedule, it is a service (docs/SPEC.md §22.1).
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// horizon bounds the search in Next. A schedule such as "0 0 30 2 *" matches no
// instant at all, and the search must end rather than spin forever.
const horizon = 5 * 365 * 24 * time.Hour

// Schedule is a parsed expression bound to a location. The zero value is not
// usable; build one with Parse.
type Schedule struct {
	// Each field is a bitmask over the values it accepts, indexed from zero.
	minute uint64 // bits 0..59
	hour   uint64 // bits 0..23
	dom    uint64 // bits 1..31
	month  uint64 // bits 1..12
	dow    uint64 // bits 0..6, Sunday first

	// A day-of-month and a day-of-week that are both restricted are combined
	// with OR, not AND — the Vixie rule. Keeping the "was it restricted" answer
	// separate is what makes that distinguishable from a field that happens to
	// list every value.
	domRestricted bool
	dowRestricted bool

	loc  *time.Location
	spec string
}

// bounds describes one field of the expression.
type bounds struct {
	name  string
	min   uint
	max   uint
	names map[string]uint
}

var (
	minuteBounds = bounds{name: "minute", min: 0, max: 59}
	hourBounds   = bounds{name: "hour", min: 0, max: 23}
	domBounds    = bounds{name: "day of month", min: 1, max: 31}
	monthBounds  = bounds{name: "month", min: 1, max: 12, names: map[string]uint{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowBounds = bounds{name: "day of week", min: 0, max: 6, names: map[string]uint{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// descriptors are the shorthand forms, expanded before parsing.
var descriptors = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// ErrEmptySpec reports an expression with nothing in it.
var ErrEmptySpec = errors.New("cron expression must not be empty")

// Parse compiles a five-field expression, or one of the @-descriptors, in loc.
// A nil location is time.UTC: a schedule with no zone is not local, it is
// absolute, and guessing the host zone would make the same file behave
// differently on two nodes.
func Parse(spec string, loc *time.Location) (*Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}

	original := strings.TrimSpace(spec)
	if original == "" {
		return nil, ErrEmptySpec
	}

	expanded := original
	if strings.HasPrefix(expanded, "@") {
		replacement, ok := descriptors[strings.ToLower(expanded)]
		if !ok {
			return nil, fmt.Errorf("unknown descriptor %q", original)
		}

		expanded = replacement
	}

	fields := strings.Fields(expanded)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expression %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", original, len(fields))
	}

	schedule := &Schedule{loc: loc, spec: original}

	var err error

	if schedule.minute, err = parseField(fields[0], minuteBounds); err != nil {
		return nil, err
	}

	if schedule.hour, err = parseField(fields[1], hourBounds); err != nil {
		return nil, err
	}

	if schedule.dom, err = parseField(fields[2], domBounds); err != nil {
		return nil, err
	}

	if schedule.month, err = parseField(fields[3], monthBounds); err != nil {
		return nil, err
	}

	if schedule.dow, err = parseField(fields[4], dowBounds); err != nil {
		return nil, err
	}

	schedule.domRestricted = fields[2] != "*"
	schedule.dowRestricted = fields[4] != "*"

	return schedule, nil
}

// Spec returns the expression as it was written.
func (s *Schedule) Spec() string { return s.spec }

// Location returns the zone the expression is evaluated in.
func (s *Schedule) Location() *time.Location { return s.loc }

// String renders the schedule for logs and errors.
func (s *Schedule) String() string { return s.spec + " " + s.loc.String() }

// Next returns the first instant strictly after t that the expression matches,
// and whether one was found within the search horizon.
//
// Daylight saving time is handled by arithmetic, not by a special case, and the
// two directions therefore behave differently on purpose:
//
//   - Spring forward: the wall clock never shows the skipped hour, so a
//     schedule inside it does not fire that day. It is skipped, not moved.
//   - Fall back: the wall clock shows the repeated hour twice, so Next returns
//     two distinct instants with the same local time. The caller deduplicates
//     them by keying on the local time (docs/SPEC.md §22.1).
func (s *Schedule) Next(t time.Time) (time.Time, bool) {
	// Start from the top of the next minute: a schedule fires on a minute
	// boundary, and the caller's clock rarely sits on one.
	current := t.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	limit := current.Add(horizon)

	for {
		if current.After(limit) {
			return time.Time{}, false
		}

		if !s.matchesMonth(current) {
			current = startOfNextMonth(current)
			continue
		}

		if !s.matchesDay(current) {
			current = startOfNextDay(current)
			continue
		}

		if !s.matchesHour(current) {
			current = startOfNextHour(current)
			continue
		}

		if !s.matchesMinute(current) {
			current = current.Add(time.Minute)
			continue
		}

		return current, true
	}
}

// Between returns every instant the expression matches in (after, until],
// capped at limit entries. It is how a restarted daemon learns what it missed
// while it was down.
//
// The cap is not a detail: a minutely schedule and a week of downtime describe
// ten thousand occurrences, and materialising them to count them is how a
// recovery path turns into an outage.
func (s *Schedule) Between(after, until time.Time, limit int) []time.Time {
	occurrences := []time.Time{}
	current := after

	for len(occurrences) < limit {
		next, ok := s.Next(current)
		if !ok || next.After(until) {
			break
		}

		occurrences = append(occurrences, next)
		current = next
	}

	return occurrences
}

func (s *Schedule) matchesMinute(t time.Time) bool { return s.minute&(1<<uint(t.Minute())) != 0 }
func (s *Schedule) matchesHour(t time.Time) bool   { return s.hour&(1<<uint(t.Hour())) != 0 }
func (s *Schedule) matchesMonth(t time.Time) bool  { return s.month&(1<<uint(t.Month())) != 0 }

// matchesDay applies the Vixie rule: when both day fields are restricted the
// day matches if either does, and otherwise only the restricted one decides.
func (s *Schedule) matchesDay(t time.Time) bool {
	domMatch := s.dom&(1<<uint(t.Day())) != 0
	dowMatch := s.dow&(1<<uint(t.Weekday())) != 0

	if s.domRestricted && s.dowRestricted {
		return domMatch || dowMatch
	}

	return domMatch && dowMatch
}

// startOfNextMonth returns midnight on the first day of the following month.
// Rebuilding the instant from wall-clock fields, rather than adding a duration,
// keeps the result correct across a DST boundary inside the skipped range.
func startOfNextMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
}

func startOfNextDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

func startOfNextHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

// parseField compiles one comma-separated field into a bitmask.
func parseField(field string, b bounds) (uint64, error) {
	var mask uint64

	for item := range strings.SplitSeq(field, ",") {
		bits, err := parseItem(strings.TrimSpace(item), b)
		if err != nil {
			return 0, err
		}

		mask |= bits
	}

	if mask == 0 {
		return 0, fmt.Errorf("%s field %q matches nothing", b.name, field)
	}

	return mask, nil
}

// parseItem compiles a single term: "*", "5", "1-5", "*/15" or "1-5/2".
func parseItem(item string, b bounds) (uint64, error) {
	if item == "" {
		return 0, fmt.Errorf("%s field has an empty term", b.name)
	}

	rangePart, stepPart, hasStep := strings.Cut(item, "/")

	step := uint(1)

	if hasStep {
		parsed, err := strconv.ParseUint(stepPart, 10, 32)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("%s step %q must be a positive number", b.name, stepPart)
		}

		step = uint(parsed)
	}

	low, high, err := parseRange(rangePart, b, hasStep)
	if err != nil {
		return 0, err
	}

	var mask uint64
	for value := low; value <= high; value += step {
		mask |= 1 << value
	}

	return mask, nil
}

// parseRange resolves the value or range part of a term into inclusive bounds.
func parseRange(part string, b bounds, hasStep bool) (uint, uint, error) {
	if part == "*" {
		return b.min, b.max, nil
	}

	lowText, highText, isRange := strings.Cut(part, "-")

	low, err := parseValue(lowText, b)
	if err != nil {
		return 0, 0, err
	}

	switch {
	case isRange:
		high, err := parseValue(highText, b)
		if err != nil {
			return 0, 0, err
		}

		if high < low {
			return 0, 0, fmt.Errorf("%s range %q is inverted", b.name, part)
		}

		return low, high, nil

	case hasStep:
		// "5/10" is the open-ended form: every tenth value from 5 upwards.
		return low, b.max, nil

	default:
		return low, low, nil
	}
}

// parseValue resolves one number or three-letter name, and rejects anything
// outside the field's range rather than clamping it.
func parseValue(text string, b bounds) (uint, error) {
	trimmed := strings.TrimSpace(text)

	if b.names != nil {
		if value, ok := b.names[strings.ToLower(trimmed)]; ok {
			return value, nil
		}
	}

	parsed, err := strconv.ParseUint(trimmed, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s value %q is not a number", b.name, text)
	}

	value := uint(parsed)

	// Sunday is both 0 and 7 in every cron dialect, and a file that says 7 must
	// not be read as "out of range".
	if b.name == dowBounds.name && value == 7 {
		return 0, nil
	}

	if value < b.min || value > b.max {
		return 0, fmt.Errorf("%s value %d is outside %d-%d", b.name, value, b.min, b.max)
	}

	return value, nil
}
