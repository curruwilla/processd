// Package config loads and validates the daemon configuration and the worker
// definitions.
//
// Both are parsed with strict YAML decoding (unknown fields are an error): a
// typo in a security-relevant key such as allow_root_processes must fail loudly
// instead of silently falling back to a default.
package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that decodes from strings such as "30s", "5m" or
// "30d".
type Duration time.Duration

// dayOrWeekPattern matches the units the standard library does not parse.
// Retention windows are naturally expressed in days, and writing 720h instead
// of 30d in a configuration file invites arithmetic mistakes.
var dayOrWeekPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([dw])$`)

// ParseDuration parses a Go duration, extended with "d" (day) and "w" (week).
func ParseDuration(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)

	if match := dayOrWeekPattern.FindStringSubmatch(trimmed); match != nil {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, fmt.Errorf("parsing duration %q: %w", raw, err)
		}

		unit := 24 * time.Hour
		if match[2] == "w" {
			unit = 7 * 24 * time.Hour
		}

		return time.Duration(value * float64(unit)), nil
	}

	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parsing duration %q: %w", raw, err)
	}

	return parsed, nil
}

// Duration returns the underlying standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the duration in Go duration syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML decodes a duration string such as "30s" or "30d".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("decoding duration: %w", err)
	}

	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}

	*d = Duration(parsed)

	return nil
}

// MarshalYAML encodes the duration back to its string form.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalJSON encodes the duration as a JSON string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON decodes a duration from a JSON string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decoding duration: %w", err)
	}

	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}

	*d = Duration(parsed)

	return nil
}

// byteUnits maps the suffixes accepted by ByteSize to their multiplier.
var byteUnits = []struct {
	suffix string
	factor int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"GB", 1e9},
	{"MB", 1e6},
	{"KB", 1e3},
	{"B", 1},
}

// ByteSize is a byte count that decodes from strings such as "32MiB".
type ByteSize int64

// Bytes returns the size as a plain byte count.
func (b ByteSize) Bytes() int64 { return int64(b) }

// String renders the size with the largest suffix that divides it exactly.
func (b ByteSize) String() string {
	for _, unit := range byteUnits {
		if int64(b) >= unit.factor && int64(b)%unit.factor == 0 {
			return strconv.FormatInt(int64(b)/unit.factor, 10) + unit.suffix
		}
	}

	return strconv.FormatInt(int64(b), 10) + "B"
}

// ParseByteSize parses a byte count with an optional IEC or SI suffix.
func ParseByteSize(raw string) (ByteSize, error) {
	trimmed := strings.TrimSpace(raw)
	for _, unit := range byteUnits {
		if !strings.HasSuffix(trimmed, unit.suffix) {
			continue
		}

		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix)), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing byte size %q: %w", raw, err)
		}

		return ByteSize(value * float64(unit.factor)), nil
	}

	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing byte size %q: %w", raw, err)
	}

	return ByteSize(value), nil
}

// UnmarshalYAML decodes a byte size such as "32MiB".
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("decoding byte size: %w", err)
	}

	parsed, err := ParseByteSize(raw)
	if err != nil {
		return err
	}

	*b = parsed

	return nil
}

// MarshalYAML encodes the byte size back to its string form.
func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

// AttemptsUnlimited is the attempt ceiling of a policy that has none. It is a
// distinct value rather than zero because zero is what an omitted key decodes
// to, and "the operator said nothing" must never mean "retry forever".
const AttemptsUnlimited = -1

// unlimitedAttempts is how an absent ceiling is written in YAML.
const unlimitedAttempts = "unlimited"

// Attempts is an attempt ceiling that decodes from a positive integer or from
// the literal "unlimited".
//
// Only a service may leave the ceiling open: a task that keeps failing has to
// stop somewhere, while a service is meant to run forever and an attempt budget
// spent over months of healthy uptime would retire it for no reason
// (docs/SPEC.md §12).
type Attempts int

// Int returns the ceiling as a plain count. It is meaningless for an unlimited
// policy, so callers check Unlimited first.
func (a Attempts) Int() int { return int(a) }

// Unlimited reports whether the policy has no attempt ceiling.
func (a Attempts) Unlimited() bool { return a == AttemptsUnlimited }

// String renders the ceiling as it is written in configuration.
func (a Attempts) String() string {
	if a.Unlimited() {
		return unlimitedAttempts
	}

	return strconv.Itoa(int(a))
}

// ParseAttempts parses an attempt ceiling written as a count or as "unlimited".
func ParseAttempts(raw string) (Attempts, error) {
	if strings.TrimSpace(raw) == unlimitedAttempts {
		return AttemptsUnlimited, nil
	}

	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("parsing attempts %q: %w", raw, err)
	}

	return Attempts(value), nil
}

// UnmarshalYAML decodes an attempt ceiling from a count or from "unlimited".
//
// The node is read as raw text rather than decoded into a string, so that both
// the bare integer and the bare word are accepted without the operator having
// to remember which one needs quoting.
func (a *Attempts) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("decoding attempts: %q is not a scalar", node.Tag)
	}

	parsed, err := ParseAttempts(node.Value)
	if err != nil {
		return err
	}

	*a = parsed

	return nil
}

// MarshalYAML encodes the ceiling back to its configured form.
func (a Attempts) MarshalYAML() (any, error) { return a.String(), nil }

// Bool returns a pointer to v. Configuration keys whose absence must stay
// distinct from an explicit "false" are decoded into a *bool, and this is how
// callers and tests supply one.
func Bool(v bool) *bool { return &v }
