package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/curruwilla/processd/internal/core"
)

// placeholderPattern matches a declared substitution point such as {{id}}.
var placeholderPattern = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)

// placeholderNames returns every placeholder name used in s, in order.
func placeholderNames(s string) []string {
	matches := placeholderPattern.FindAllStringSubmatch(s, -1)

	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}

	return names
}

// Resolved is a worker template with request parameters applied.
type Resolved struct {
	Args []string
	Lock string
}

// Resolve applies request parameters to the worker template (docs/SPEC.md §5.3).
//
// Substitution happens inside argv elements only: it never creates, splits or
// merges elements, so a parameter value containing spaces stays exactly one
// argument. There is no word splitting and no free-form argument passing — a
// value that is not declared as a param cannot reach the process.
func (w *Worker) Resolve(params map[string]string) (Resolved, error) {
	values, err := w.resolveParams(params)
	if err != nil {
		return Resolved{}, err
	}

	args := make([]string, 0, len(w.Args))

	for _, arg := range w.Args {
		names := placeholderNames(arg)

		// An argv element that is nothing but an absent optional placeholder is
		// dropped instead of being passed as an empty string.
		if len(names) == 1 && arg == "{{"+names[0]+"}}" {
			value, ok := values[names[0]]
			if !ok {
				continue
			}

			args = append(args, value)

			continue
		}

		args = append(args, substitute(arg, values))
	}

	return Resolved{Args: args, Lock: substitute(w.Lock, values)}, nil
}

// resolveParams validates the request parameters against the declaration and
// returns the values to substitute. Absent optional params without a default
// are omitted from the result.
func (w *Worker) resolveParams(params map[string]string) (map[string]string, error) {
	for name := range params {
		if _, declared := w.Params[name]; !declared {
			return nil, &core.ParamError{Param: name, Reason: "is not declared by this worker"}
		}
	}

	values := make(map[string]string, len(w.Params))

	for name, param := range w.Params {
		value, provided := params[name]

		switch {
		case provided:
		case param.Required:
			return nil, &core.ParamError{Param: name, Reason: "is required"}
		case param.Default != "":
			value = param.Default
		default:
			continue
		}

		if err := param.validate(name, value); err != nil {
			return nil, err
		}

		values[name] = value
	}

	return values, nil
}

// validate checks one value against the declared shape. It runs before any
// process starts, so a rejected value never reaches exec.
func (p Param) validate(name, value string) error {
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, value) {
		return &core.ParamError{
			Param:  name,
			Reason: "must be one of " + strings.Join(p.Enum, ", "),
		}
	}

	if p.Pattern == "" {
		return nil
	}

	// compiled is populated by Worker.Validate at load time; the fallback keeps
	// the check correct for workers built in tests.
	matcher := p.compiled
	if matcher == nil {
		compiled, err := regexp.Compile(p.Pattern)
		if err != nil {
			return fmt.Errorf("compiling pattern for param %q: %w", name, err)
		}

		matcher = compiled
	}

	if !matcher.MatchString(value) {
		return &core.ParamError{Param: name, Reason: "does not match the declared pattern"}
	}

	return nil
}

func substitute(template string, values map[string]string) string {
	if template == "" {
		return ""
	}

	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		return values[name]
	})
}
