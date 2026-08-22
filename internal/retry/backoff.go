// Package retry holds the restart policy: when a failed attempt is retried and
// how long the daemon waits first.
package retry

import (
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/curruwilla/processd/internal/config"
)

// Delay returns how long to wait before the given attempt (1-based).
//
// Jitter is not decoration: without it, every execution that failed for the
// same external reason retries in lockstep and reproduces the original spike.
func Delay(b config.Backoff, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	initial := b.Initial.Duration()
	ceiling := b.Max.Duration()

	var delay time.Duration

	switch b.Type {
	case config.BackoffFixed:
		delay = initial
	case config.BackoffLinear:
		delay = time.Duration(attempt) * initial
	default:
		multiplier := b.Multiplier
		if multiplier <= 0 {
			multiplier = 2
		}

		delay = time.Duration(float64(initial) * math.Pow(multiplier, float64(attempt-1)))
	}

	if ceiling > 0 && delay > ceiling {
		delay = ceiling
	}

	return applyJitter(delay, b.Jitter)
}

func applyJitter(delay time.Duration, jitter float64) time.Duration {
	if jitter <= 0 || delay <= 0 {
		return delay
	}

	// Spread uniformly over [delay*(1-jitter), delay*(1+jitter)].
	//nolint:gosec // spreading retries needs no cryptographic randomness
	factor := 1 + jitter*(2*rand.Float64()-1)

	jittered := time.Duration(float64(delay) * factor)
	if jittered < 0 {
		return 0
	}

	return jittered
}

// Attempts returns how many attempts a worker is allowed to make in total, or
// config.AttemptsUnlimited when the policy sets no ceiling.
func Attempts(r config.Retry) int { return r.EffectiveAttempts() }

// Allowed reports whether another attempt may run after a failure of the given
// class, taking the attempt counter into account.
func Allowed(r config.Retry, trigger config.RetryTrigger, attempt int) bool {
	if !r.IsEnabled() {
		return false
	}

	if !slices.Contains(r.RetryOn, trigger) {
		return false
	}

	ceiling := Attempts(r)
	if ceiling == config.AttemptsUnlimited {
		return true
	}

	return attempt < ceiling
}

// CounterReset reports whether an attempt ran long enough for the attempt
// counter to be forgiven. Without this, a long-lived execution accumulates
// attempts over days and ends up FAILED without ever having a real problem.
func CounterReset(r config.Retry, ran time.Duration) bool {
	reset := r.ResetAfter.Duration()

	return reset > 0 && ran >= reset
}

// Succeeded reports whether an exit code counts as success for the worker. It
// is only ever asked about a task: no exit of a service is a success, which is
// why a service may not declare success_exit_codes at all.
func Succeeded(r config.Retry, exitCode int) bool {
	codes := r.SuccessExitCodes
	if len(codes) == 0 {
		codes = []int{0}
	}

	return slices.Contains(codes, exitCode)
}

// Fatal reports whether an exit code must skip retries entirely.
func Fatal(r config.Retry, exitCode int) bool {
	return slices.Contains(r.NoRetryExitCodes, exitCode)
}
