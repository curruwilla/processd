package retry

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

func TestAllowed(t *testing.T) {
	t.Parallel()

	policy := config.Retry{
		Enabled:     config.Bool(true),
		MaxAttempts: 3,
		RetryOn:     []config.RetryTrigger{config.RetryOnNonZeroExit, config.RetryOnSignal},
	}

	tests := []struct {
		name    string
		policy  config.Retry
		trigger config.RetryTrigger
		attempt int
		want    bool
	}{
		{name: "first failure retries", policy: policy, trigger: config.RetryOnNonZeroExit, attempt: 1, want: true},
		{name: "last attempt does not", policy: policy, trigger: config.RetryOnNonZeroExit, attempt: 3, want: false},
		{
			name:    "an untriggered class does not",
			policy:  policy,
			trigger: config.RetryOnTimeout,
			attempt: 1,
			want:    false,
		},
		{
			name:    "a disabled policy never retries",
			policy:  config.Retry{MaxAttempts: 5, RetryOn: []config.RetryTrigger{config.RetryOnNonZeroExit}},
			trigger: config.RetryOnNonZeroExit,
			attempt: 1,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Allowed(tt.policy, tt.trigger, tt.attempt); got != tt.want {
				t.Errorf("Allowed() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSucceeded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policy   config.Retry
		exitCode int
		want     bool
	}{
		{name: "zero is success by default", policy: config.Retry{}, exitCode: 0, want: true},
		{name: "non-zero is not", policy: config.Retry{}, exitCode: 1, want: false},
		{
			name:     "a worker may declare other successes",
			policy:   config.Retry{SuccessExitCodes: []int{0, 75}},
			exitCode: 75,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Succeeded(tt.policy, tt.exitCode); got != tt.want {
				t.Errorf("Succeeded() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestFatal(t *testing.T) {
	t.Parallel()

	policy := config.Retry{NoRetryExitCodes: []int{64, 78}}

	if !Fatal(policy, 64) {
		t.Error("Fatal(64) = false, want true")
	}

	if Fatal(policy, 1) {
		t.Error("Fatal(1) = true, want false")
	}
}

func TestCounterReset(t *testing.T) {
	t.Parallel()

	policy := config.Retry{ResetAfter: config.Duration(10 * time.Minute)}

	tests := []struct {
		name string
		ran  time.Duration
		want bool
	}{
		{name: "a long run forgives the counter", ran: 11 * time.Minute, want: true},
		{name: "a short run does not", ran: time.Minute, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := CounterReset(policy, tt.ran); got != tt.want {
				t.Errorf("CounterReset(%s) = %t, want %t", tt.ran, got, tt.want)
			}
		})
	}

	if CounterReset(config.Retry{}, time.Hour) {
		t.Error("CounterReset without reset_after = true, want false")
	}
}

// A service is meant to run forever, so its policy has no attempt budget to
// exhaust: an uptime measured in months must not retire it (docs/SPEC.md §12).
func TestAllowed_Unlimited(t *testing.T) {
	t.Parallel()

	policy := config.Retry{
		Enabled:     config.Bool(true),
		MaxAttempts: config.AttemptsUnlimited,
		RetryOn:     []config.RetryTrigger{config.RetryOnExit},
	}

	if got := Attempts(policy); got != config.AttemptsUnlimited {
		t.Errorf("Attempts() = %d, want %d", got, config.AttemptsUnlimited)
	}

	for _, attempt := range []int{1, 2, 10_000} {
		if !Allowed(policy, config.RetryOnExit, attempt) {
			t.Errorf("Allowed(attempt %d) = false, want true", attempt)
		}
	}

	// An unlimited ceiling is not a licence to retry a class the policy never
	// opted into.
	if Allowed(policy, config.RetryOnTimeout, 1) {
		t.Error("Allowed(timeout) = true, want false for a trigger outside retry_on")
	}
}

// A task keeps a bounded budget even though the unlimited sentinel now exists.
func TestAllowed_TaskCeilingUnchanged(t *testing.T) {
	t.Parallel()

	policy := config.Retry{
		Enabled:     config.Bool(true),
		MaxAttempts: 2,
		RetryOn:     []config.RetryTrigger{config.RetryOnNonZeroExit},
	}

	if !Allowed(policy, config.RetryOnNonZeroExit, 1) {
		t.Error("Allowed(attempt 1) = false, want true")
	}

	if Allowed(policy, config.RetryOnNonZeroExit, 2) {
		t.Error("Allowed(attempt 2) = true, want false at the ceiling")
	}
}
