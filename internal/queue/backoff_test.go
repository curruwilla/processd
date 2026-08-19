package queue

import (
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
)

func TestDelay(t *testing.T) {
	t.Parallel()

	base := config.Backoff{
		Initial:    config.Duration(5 * time.Second),
		Max:        config.Duration(time.Minute),
		Multiplier: 2,
	}

	tests := []struct {
		name        string
		backoffType config.BackoffType
		attempt     int
		want        time.Duration
	}{
		{name: "exponential first attempt", backoffType: config.BackoffExponential, attempt: 1, want: 5 * time.Second},
		{name: "exponential second attempt", backoffType: config.BackoffExponential, attempt: 2, want: 10 * time.Second},
		{name: "exponential third attempt", backoffType: config.BackoffExponential, attempt: 3, want: 20 * time.Second},
		{name: "exponential is capped", backoffType: config.BackoffExponential, attempt: 9, want: time.Minute},
		{name: "linear grows by attempt", backoffType: config.BackoffLinear, attempt: 3, want: 15 * time.Second},
		{name: "fixed ignores the attempt", backoffType: config.BackoffFixed, attempt: 7, want: 5 * time.Second},
		{name: "attempt zero is treated as the first", backoffType: config.BackoffExponential, attempt: 0, want: 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backoff := base
			backoff.Type = tt.backoffType

			if got := Delay(backoff, tt.attempt); got != tt.want {
				t.Errorf("Delay(%s, %d) = %s, want %s", tt.backoffType, tt.attempt, got, tt.want)
			}
		})
	}
}

func TestDelay_jitterStaysWithinBounds(t *testing.T) {
	t.Parallel()

	backoff := config.Backoff{
		Type:       config.BackoffFixed,
		Initial:    config.Duration(10 * time.Second),
		Max:        config.Duration(time.Minute),
		Multiplier: 2,
		Jitter:     0.2,
	}

	low := 8 * time.Second
	high := 12 * time.Second
	spread := map[time.Duration]bool{}

	for range 100 {
		got := Delay(backoff, 1)

		if got < low || got > high {
			t.Fatalf("Delay() = %s, want it within [%s, %s]", got, low, high)
		}

		spread[got] = true
	}

	// Jitter exists to desynchronise retries: a constant value would defeat it.
	if len(spread) < 2 {
		t.Error("jittered delay never varied, want a spread of values")
	}
}
