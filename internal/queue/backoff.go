package queue

import (
	"math"
	"math/rand/v2"
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
