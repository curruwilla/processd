package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// parseFilter builds a listing filter from the query string. Paging is always
// cursor-based: an unbounded listing would grow with the retained history.
func parseFilter(r *http.Request) (store.Filter, error) {
	query := r.URL.Query()

	filter := store.Filter{
		Worker: query.Get("worker"),
		Lock:   query.Get("lock"),
		Cursor: query.Get("cursor"),
		Limit:  defaultListLimit,
	}

	if raw := query.Get("type"); raw != "" {
		switch core.Type(raw) {
		case core.TypeTask, core.TypeService:
			filter.Type = core.Type(raw)
		default:
			return store.Filter{}, badRequest("type_unknown", fmt.Sprintf("type %q is unknown", raw))
		}
	}

	known := core.States()

	for _, raw := range query["status"] {
		state := core.State(raw)
		if !slices.Contains(known, state) {
			return store.Filter{}, badRequest("status_unknown", fmt.Sprintf("status %q is unknown", raw))
		}

		filter.States = append(filter.States, state)
	}

	after, err := parseTime(query.Get("created_after"))
	if err != nil {
		return store.Filter{}, err
	}

	before, err := parseTime(query.Get("created_before"))
	if err != nil {
		return store.Filter{}, err
	}

	filter.CreatedAfter = after
	filter.CreatedBefore = before

	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return store.Filter{}, badRequest("limit_invalid", fmt.Sprintf("limit %q is not a positive integer", raw))
		}

		filter.Limit = min(limit, maxListLimit)
	}

	return filter, nil
}

func parseTime(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, badRequest("time_invalid", fmt.Sprintf("%q is not an RFC 3339 timestamp", raw))
	}

	return &parsed, nil
}
