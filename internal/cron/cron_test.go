package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string, loc *time.Location) *Schedule {
	t.Helper()

	schedule, err := Parse(spec, loc)
	if err != nil {
		t.Fatalf("Parse(%q) returned %v", spec, err)
	}

	return schedule
}

func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec string
	}{
		{name: "empty", spec: "   "},
		{name: "too few fields", spec: "0 3 * *"},
		{name: "too many fields", spec: "0 3 * * * *"},
		{name: "unknown descriptor", spec: "@fortnightly"},
		{name: "minute out of range", spec: "60 3 * * *"},
		{name: "hour out of range", spec: "0 24 * * *"},
		{name: "day zero", spec: "0 0 0 * *"},
		{name: "month out of range", spec: "0 0 1 13 *"},
		{name: "weekday out of range", spec: "0 0 * * 8"},
		{name: "inverted range", spec: "0 0 * * 5-1"},
		{name: "zero step", spec: "*/0 * * * *"},
		{name: "not a number", spec: "soon * * * *"},
		{name: "empty term", spec: "0,,5 * * * *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse(tt.spec, time.UTC); err == nil {
				t.Fatalf("Parse(%q) accepted an invalid expression", tt.spec)
			}
		})
	}
}

func TestSchedule_Next(t *testing.T) {
	t.Parallel()

	// A Wednesday.
	base := time.Date(2026, 8, 19, 10, 30, 15, 0, time.UTC)

	tests := []struct {
		name string
		spec string
		from time.Time
		want time.Time
	}{
		{
			name: "every minute rounds up to the next boundary",
			spec: "* * * * *",
			from: base,
			want: time.Date(2026, 8, 19, 10, 31, 0, 0, time.UTC),
		},
		{
			name: "daily at three moves to tomorrow",
			spec: "0 3 * * *",
			from: base,
			want: time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "hourly descriptor",
			spec: "@hourly",
			from: base,
			want: time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC),
		},
		{
			name: "step within the hour",
			spec: "*/15 * * * *",
			from: base,
			want: time.Date(2026, 8, 19, 10, 45, 0, 0, time.UTC),
		},
		{
			name: "list of minutes picks the nearest",
			spec: "0,20,40 * * * *",
			from: base,
			want: time.Date(2026, 8, 19, 10, 40, 0, 0, time.UTC),
		},
		{
			name: "weekday by name",
			spec: "0 9 * * fri",
			from: base,
			want: time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "sunday as seven",
			spec: "0 9 * * 7",
			from: base,
			want: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "month by name rolls into next year",
			spec: "0 0 1 jan *",
			from: base,
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "range of hours on weekdays",
			spec: "0 9-17 * * 1-5",
			from: time.Date(2026, 8, 21, 17, 30, 0, 0, time.UTC),
			want: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "open ended step",
			spec: "5/20 * * * *",
			from: base,
			want: time.Date(2026, 8, 19, 10, 45, 0, 0, time.UTC),
		},
		{
			name: "exact boundary still advances",
			spec: "30 10 * * *",
			from: time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC),
			want: time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := mustParse(t, tt.spec, time.UTC).Next(tt.from)
			if !ok {
				t.Fatalf("Next(%v) found no occurrence", tt.from)
			}

			if !got.Equal(tt.want) {
				t.Fatalf("Next(%v) = %v, want %v", tt.from, got, tt.want)
			}
		})
	}
}

// TestSchedule_Next_VixieDayRule pins the rule that surprises everyone: two
// restricted day fields are OR, not AND.
func TestSchedule_Next_VixieDayRule(t *testing.T) {
	t.Parallel()

	// 2026-08-19 is a Wednesday; the 20th is a Thursday.
	from := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	both := mustParse(t, "0 0 20 * fri", time.UTC)

	got, ok := both.Next(from)
	if !ok {
		t.Fatal("Next found no occurrence")
	}

	// The 20th matches day-of-month even though it is not a Friday.
	want := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (day-of-month must match on its own)", got, want)
	}

	// With only day-of-week restricted, the day-of-month field must not narrow
	// the result.
	onlyDOW := mustParse(t, "0 0 * * fri", time.UTC)

	got, ok = onlyDOW.Next(from)
	if !ok {
		t.Fatal("Next found no occurrence")
	}

	want = time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestSchedule_Next_Unsatisfiable(t *testing.T) {
	t.Parallel()

	// 30 February never happens.
	schedule := mustParse(t, "0 0 30 2 *", time.UTC)

	if _, ok := schedule.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("Next found an occurrence for 30 February")
	}
}

// TestSchedule_Next_SpringForward pins the documented behaviour: a schedule
// inside the skipped hour does not fire that day.
func TestSchedule_Next_SpringForward(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}

	// On 2026-03-08 the clock jumps 02:00 -> 03:00 in New York.
	schedule := mustParse(t, "30 2 * * *", loc)

	from := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)

	got, ok := schedule.Next(from)
	if !ok {
		t.Fatal("Next found no occurrence")
	}

	// 02:30 on the 8th does not exist, so the next firing is on the 9th.
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (the skipped hour must not fire)", got, want)
	}
}

// TestSchedule_Next_FallBack pins the other direction: the repeated hour yields
// two distinct instants that share a local time. Deduplicating them is the
// caller's job, by key, not the parser's.
func TestSchedule_Next_FallBack(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}

	// On 2026-11-01 the clock falls back 02:00 -> 01:00 in New York.
	schedule := mustParse(t, "30 1 * * *", loc)

	first, ok := schedule.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, loc))
	if !ok {
		t.Fatal("Next found no first occurrence")
	}

	second, ok := schedule.Next(first)
	if !ok {
		t.Fatal("Next found no second occurrence")
	}

	if second.Sub(first).Round(time.Minute) != time.Hour {
		t.Fatalf("second occurrence is %v after the first, want 1h", second.Sub(first))
	}

	if first.Format("15:04") != second.Format("15:04") {
		t.Fatalf("occurrences render as %q and %q, want the same local time",
			first.Format("15:04"), second.Format("15:04"))
	}
}

func TestSchedule_Between(t *testing.T) {
	t.Parallel()

	schedule := mustParse(t, "0 * * * *", time.UTC)

	after := time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC)
	until := time.Date(2026, 8, 19, 14, 30, 0, 0, time.UTC)

	got := schedule.Between(after, until, 10)
	if len(got) != 4 {
		t.Fatalf("Between returned %d occurrences, want 4: %v", len(got), got)
	}

	if !got[0].Equal(time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("first occurrence = %v", got[0])
	}

	// The cap must bound the answer, not the window.
	capped := schedule.Between(after, until, 2)
	if len(capped) != 2 {
		t.Fatalf("Between with a cap of 2 returned %d occurrences", len(capped))
	}
}

func TestParse_NilLocationIsUTC(t *testing.T) {
	t.Parallel()

	if got := mustParse(t, "@daily", nil).Location(); got != time.UTC {
		t.Fatalf("Location() = %v, want UTC", got)
	}
}
