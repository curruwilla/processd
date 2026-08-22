package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/curruwilla/processd/internal/fleet"
)

// fleetBodyLimit bounds what the hub reads from one node while merging a
// listing. A node is not hostile, but it is remote.
const fleetBodyLimit = 8 << 20

// allNodes is the value of ?node= that means "merge every node".
const allNodes = "*"

// processesPath is the endpoint a hub reads and dispatches to on a node.
const processesPath = "/v1/processes"

// fleetListing is the shape the hub decodes from a node.
//
// The items stay maps rather than a typed struct on purpose: a node one release
// ahead may report a field this hub has never heard of, and dropping it on the
// floor would make the hub the reason a new field is invisible.
type fleetListing struct {
	Items      []map[string]any `json:"items"`
	NextCursor string           `json:"next_cursor"`
}

// listFleetNodes reports the last poll of every node.
func (s *Server) listFleetNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, s.fleet.Statuses())
}

// proxyToNode forwards one call to the node the client named.
//
// The path is resolved before it is used, so the hub cannot be talked into
// asking a node for something outside its API, and the node applies its own
// rules to the hub's token regardless of what arrives here.
func (s *Server) proxyToNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")

	proxy, ok := s.fleet.Proxy(name)
	if !ok {
		writeError(w, s.log, &apiError{
			Status:  http.StatusNotFound,
			Code:    "node_unknown",
			Message: fmt.Sprintf("node %q is not part of this fleet", name),
		})

		return
	}

	target, ok := fleet.TargetPath(r.PathValue("path"))
	if !ok {
		writeError(w, s.log, badRequest("path_invalid", "the proxied path must stay inside /v1"))
		return
	}

	forwarded := r.Clone(r.Context())
	forwarded.URL.Path = target
	forwarded.RequestURI = ""

	proxy.ServeHTTP(w, forwarded)
}

// listFleetProcesses answers GET /v1/processes?node=... by asking the nodes.
//
// A merged page is a snapshot of the newest items each node had, not a cursor
// into a combined history: there is no ordering across nodes to page through,
// and inventing one would be a distributed index nobody asked for. Paging deeper
// means naming a single node.
func (s *Server) listFleetProcesses(w http.ResponseWriter, r *http.Request, node string) {
	if node != allNodes && !s.fleet.Has(node) {
		writeError(w, s.log, &apiError{
			Status:  http.StatusNotFound,
			Code:    "node_unknown",
			Message: fmt.Sprintf("node %q is not part of this fleet", node),
		})

		return
	}

	filter, err := parseFilter(r)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	query := r.URL.Query()
	query.Del("node")

	names := []string{node}
	if node == allNodes {
		names = s.fleet.Names()
	}

	merged, failures := s.gather(r, names, query)

	// One unreachable node degrades the page rather than failing it: the point
	// of the view is the nodes that did answer.
	for name, reason := range failures {
		s.log.Warn("listing from a fleet node", slog.String("node", name), slog.Any("error", reason))
	}

	sort.SliceStable(merged.items, func(i, j int) bool {
		return createdAtOf(merged.items[i]).After(createdAtOf(merged.items[j]))
	})

	if len(merged.items) > filter.Limit {
		merged.items = merged.items[:filter.Limit]
	}

	response := map[string]any{"items": merged.items}

	// A single node keeps its own cursor, because paging one node is ordinary
	// paging. A merged view has none to give.
	if node != allNodes {
		response["next_cursor"] = merged.cursor
	}

	if len(failures) > 0 {
		unreachable := make([]string, 0, len(failures))
		for name := range failures {
			unreachable = append(unreachable, name)
		}

		slices.Sort(unreachable)

		response["unreachable"] = unreachable
	}

	writeJSON(w, s.log, http.StatusOK, response)
}

// gathered collects what the nodes returned.
type gathered struct {
	items  []map[string]any
	cursor string
}

// gather queries the nodes in parallel and tags every item with its origin.
func (s *Server) gather(r *http.Request, names []string, query url.Values) (gathered, map[string]error) {
	var (
		mu       sync.Mutex
		result   gathered
		failures = map[string]error{}
		wg       sync.WaitGroup
	)

	result.items = []map[string]any{}

	for _, name := range names {
		wg.Go(func() {
			listing, err := s.fetchListing(r, name, query)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				failures[name] = err
				return
			}

			for _, item := range listing.Items {
				// The origin is what makes a merged row actionable: without it,
				// an ID in the list cannot be looked up again.
				item["node"] = name
				result.items = append(result.items, item)
			}

			result.cursor = listing.NextCursor
		})
	}

	wg.Wait()

	return result, failures
}

// fetchListing reads one page of executions from one node.
func (s *Server) fetchListing(r *http.Request, name string, query url.Values) (fleetListing, error) {
	resp, err := s.fleet.Get(r.Context(), name, processesPath, query)
	if err != nil {
		return fleetListing{}, err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		return fleetListing{}, fmt.Errorf("node %q answered %s", name, resp.Status)
	}

	var listing fleetListing
	if err := json.NewDecoder(io.LimitReader(resp.Body, fleetBodyLimit)).Decode(&listing); err != nil {
		return fleetListing{}, fmt.Errorf("decoding the listing from node %q: %w", name, err)
	}

	return listing, nil
}

// createdAtOf reads the sort key out of an untyped item. An item whose
// timestamp this hub cannot parse sorts last rather than breaking the page.
func createdAtOf(item map[string]any) time.Time {
	raw, ok := item["created_at"].(string)
	if !ok {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

// dispatchToNode forwards a submission to the node the client named.
//
// The hub is an address book, not a scheduler: it does not choose the node, it
// keeps no record of what it forwarded, and it never decides on its own what
// became of it. Everything a placement decision would need is deliberately
// absent (docs/SPEC.md §22.5).
func (s *Server) dispatchToNode(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if !s.fleet.Has(name) {
		writeError(w, s.log, &apiError{
			Status:  http.StatusNotFound,
			Code:    "node_unknown",
			Message: fmt.Sprintf("node %q is not part of this fleet", name),
		})

		return
	}

	forwarded, err := withoutNode(body)
	if err != nil {
		writeError(w, s.log, badRequest("body_invalid", "the request body is not an object"))
		return
	}

	proxy, _ := s.fleet.Proxy(name)

	request := r.Clone(r.Context())
	request.URL.Path = processesPath
	request.RequestURI = ""
	request.Body = io.NopCloser(bytes.NewReader(forwarded))
	request.ContentLength = int64(len(forwarded))
	request.Header.Set("Content-Type", "application/json")

	// A timeout is not an answer. The proxy's error handler reports 502, and the
	// wrapper below turns that into the one thing a caller must not misread as
	// "nothing happened".
	proxy.ServeHTTP(&dispatchWriter{ResponseWriter: w, node: name, log: s.log}, request)
}

// withoutNode strips the routing field before the body reaches the node, which
// has no idea what a fleet is and would refuse an unknown field.
func withoutNode(body []byte) ([]byte, error) {
	var decoded map[string]any

	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decoding the dispatch body: %w", err)
	}

	delete(decoded, "node")

	return json.Marshal(decoded)
}

// dispatchWriter rewrites the answer of a dispatch so the caller can tell the
// three cases apart: it ran, it was refused, or nobody knows.
//
// The last one is the reason this type exists. A node that did not answer has
// not said no: the execution may be running right now. Reporting that as a
// failure is how a caller is talked into starting the same work twice.
type dispatchWriter struct {
	http.ResponseWriter

	node    string
	log     *slog.Logger
	written bool

	// replaced is set once this writer has answered on its own. The proxy's
	// error handler writes a body of its own right after the status, and letting
	// it through would append a second JSON document to the first.
	replaced bool
}

func (d *dispatchWriter) WriteHeader(status int) {
	if d.written {
		return
	}

	d.written = true

	d.Header().Set("X-Processd-Node", d.node)

	if status != http.StatusBadGateway {
		d.ResponseWriter.WriteHeader(status)
		return
	}

	d.replaced = true

	d.log.Warn("dispatch to a fleet node had no answer", slog.String("node", d.node))

	d.Header().Set("Content-Type", "application/json")
	d.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)

	_, _ = io.WriteString(d.ResponseWriter, `{"error":{"code":"dispatch_unknown","message":`+
		`"the node did not answer: the execution may or may not have started. `+
		`Retry with the same Idempotency-Key rather than assuming it did not."}}`)
}

func (d *dispatchWriter) Write(p []byte) (int, error) {
	if !d.written {
		d.WriteHeader(http.StatusOK)
	}

	// The answer has already been written in full.
	if d.replaced {
		return len(p), nil
	}

	return d.ResponseWriter.Write(p)
}
