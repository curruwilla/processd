// Package fleet reads other nodes.
//
// It is the whole of what a hub does: aggregate, proxy, and never write
// (docs/SPEC.md §22.3). The aggregation is pull-based, so a node needs no
// configuration to be part of a fleet and does not know that it is; a hub that
// is down leaves every node running exactly as it was.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/curruwilla/processd/internal/config"
)

// maxBodyBytes bounds what the hub reads from a node in one aggregation call.
// A node is not hostile, but it is remote, and a remote answer is untrusted
// input as far as memory is concerned.
const maxBodyBytes = 8 << 20

// Status is the last thing a poll learned about a node.
//
// It is deliberately a snapshot and not a truth: the worst a stale one can
// cause is an out-of-date panel, because nothing is ever decided from it.
type Status struct {
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Version   string         `json:"version,omitempty"`
	LastSeen  *time.Time     `json:"last_seen,omitempty"`
	Stats     map[string]any `json:"stats,omitempty"`
}

// node is one aggregated node and everything needed to read it.
type node struct {
	name      string
	url       string
	tokenFile string
	proxy     *httputil.ReverseProxy

	// mu guards token, which is re-read on every poll so that rotating a node's
	// token takes effect within one interval instead of at the next restart.
	mu    sync.RWMutex
	token string
}

func (n *node) bearer() string {
	n.mu.RLock()
	defer n.mu.RUnlock()

	return n.token
}

func (n *node) setToken(token string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.token = token
}

// Fleet polls the configured nodes and proxies reads to them.
type Fleet struct {
	nodes  []*node
	byName map[string]*node

	client   *http.Client
	interval time.Duration
	timeout  time.Duration
	log      *slog.Logger

	mu     sync.RWMutex
	status map[string]Status
}

// New wires a fleet from the configuration. It returns nil when this daemon
// aggregates nothing, which is the ordinary single-node case.
func New(cfg config.Fleet, log *slog.Logger) (*Fleet, error) {
	if !cfg.IsSet() {
		return nil, nil
	}

	f := &Fleet{
		byName:   map[string]*node{},
		client:   &http.Client{Timeout: cfg.Timeout.Duration()},
		interval: cfg.PollInterval.Duration(),
		timeout:  cfg.Timeout.Duration(),
		log:      log,
		status:   map[string]Status{},
	}

	for _, declared := range cfg.Nodes {
		target, err := url.Parse(strings.TrimSuffix(declared.URL, "/"))
		if err != nil {
			return nil, fmt.Errorf("fleet node %q: %w", declared.Name, err)
		}

		built := &node{name: declared.Name, url: target.String(), tokenFile: declared.TokenFile}

		// Fail closed at boot: a token that cannot be read now will not start
		// working later, and a fleet that silently drops a node is worse than
		// one that refuses to start.
		if err := built.reloadToken(); err != nil {
			return nil, err
		}

		built.proxy = newProxy(target, built, log)

		f.nodes = append(f.nodes, built)
		f.byName[declared.Name] = built
		f.status[declared.Name] = Status{Name: declared.Name, URL: built.url}
	}

	return f, nil
}

// reloadToken re-reads the node's token from disk.
func (n *node) reloadToken() error {
	raw, err := os.ReadFile(n.tokenFile)
	if err != nil {
		return fmt.Errorf("reading token file for node %q: %w", n.name, err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return fmt.Errorf("token file %q for node %q is empty", n.tokenFile, n.name)
	}

	n.setToken(token)

	return nil
}

// Names returns the configured node names, in configuration order.
func (f *Fleet) Names() []string {
	names := make([]string, 0, len(f.nodes))
	for _, n := range f.nodes {
		names = append(names, n.name)
	}

	return names
}

// Has reports whether the named node is configured.
func (f *Fleet) Has(name string) bool {
	_, ok := f.byName[name]

	return ok
}

// Statuses returns the last poll result for every node, in configuration order.
func (f *Fleet) Statuses() []Status {
	f.mu.RLock()
	defer f.mu.RUnlock()

	statuses := make([]Status, 0, len(f.nodes))
	for _, n := range f.nodes {
		statuses = append(statuses, f.status[n.name])
	}

	return statuses
}

// Run polls every node until the context is cancelled.
func (f *Fleet) Run(ctx context.Context) error {
	f.pollAll(ctx)

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.pollAll(ctx)
		}
	}
}

// pollAll refreshes every node in parallel. One unreachable node must not delay
// the others, which is the whole reason the poll fans out.
func (f *Fleet) pollAll(ctx context.Context) {
	var wg sync.WaitGroup

	for _, n := range f.nodes {
		wg.Go(func() { f.poll(ctx, n) })
	}

	wg.Wait()
}

// poll asks one node how it is doing and records the answer.
func (f *Fleet) poll(ctx context.Context, n *node) {
	if err := n.reloadToken(); err != nil {
		f.record(Status{Name: n.name, URL: n.url, Error: err.Error()})
		return
	}

	status := Status{Name: n.name, URL: n.url}

	var health struct {
		Version string `json:"version"`
	}

	if err := f.decode(ctx, n, "/v1/health", &health); err != nil {
		status.Error = err.Error()
		f.record(status)

		return
	}

	status.Version = health.Version

	// Stats are decoded loosely on purpose: a node one release ahead may report
	// a counter this hub has never heard of, and the panel should show it rather
	// than fail on it.
	stats := map[string]any{}
	if err := f.decode(ctx, n, "/v1/stats", &stats); err != nil {
		status.Error = err.Error()
		f.record(status)

		return
	}

	seen := time.Now().UTC()
	status.Stats = stats
	status.Reachable = true
	status.LastSeen = &seen

	f.record(status)
}

// record stores a poll result, keeping the last successful sighting.
func (f *Fleet) record(status Status) {
	f.mu.Lock()

	previous, known := f.status[status.Name]

	if known && status.LastSeen == nil {
		status.LastSeen = previous.LastSeen
		status.Version = previous.Version
		status.Stats = previous.Stats
	}

	f.status[status.Name] = status

	f.mu.Unlock()

	// Logged when the answer changes, not on every poll. A node that is down for
	// a day at a ten-second interval is one fact, and eight thousand copies of
	// it bury everything else in the log.
	if known && previous.Reachable == status.Reachable {
		return
	}

	if !status.Reachable {
		f.log.Warn("fleet node is unreachable",
			slog.String("node", status.Name), slog.String("error", status.Error))

		return
	}

	if known {
		f.log.Info("fleet node is answering again", slog.String("node", status.Name))
	}
}

// decode performs one authenticated GET against a node and decodes the body.
func (f *Fleet) decode(ctx context.Context, n *node, endpoint string, out any) error {
	resp, err := f.Get(ctx, n.name, endpoint, nil)
	if err != nil {
		return err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("node answered %s for %s", resp.Status, endpoint)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("decoding %s from node: %w", endpoint, err)
	}

	return nil
}

// ErrUnknownNode reports a node that is not part of this fleet.
var ErrUnknownNode = errors.New("unknown fleet node")

// Get performs one authenticated GET against a node. The caller owns the body.
func (f *Fleet) Get(ctx context.Context, name, endpoint string, query url.Values) (*http.Response, error) {
	n, ok := f.byName[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, ErrUnknownNode)
	}

	target := n.url + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for node %q: %w", name, err)
	}

	req.Header.Set("Authorization", "Bearer "+n.bearer())

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling node %q: %w", name, err)
	}

	return resp, nil
}

// Proxy returns the reverse proxy for a node, or false when it is not part of
// this fleet.
func (f *Fleet) Proxy(name string) (http.Handler, bool) {
	n, ok := f.byName[name]
	if !ok {
		return nil, false
	}

	return n.proxy, true
}

// newProxy builds the reverse proxy for one node.
//
// It streams rather than buffers, because the most useful thing to proxy is a
// log that has not finished being written, and it replaces the caller's
// credential with the hub's own: a client authenticated to the hub, never to
// the node behind it.
func newProxy(target *url.URL, n *node, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// -1 flushes as bytes arrive, which is what a Server-Sent Events stream
		// needs; anything else delivers a live log all at once, at the end.
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = target.Scheme
			r.Out.URL.Host = target.Host
			r.Out.Host = target.Host

			r.Out.Header.Del("Authorization")
			r.Out.Header.Set("Authorization", "Bearer "+n.bearer())
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Warn("proxying to a fleet node", slog.String("node", n.name), slog.Any("error", err))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)

			_, _ = io.WriteString(w, `{"error":{"code":"node_unreachable","message":"the node did not answer"}}`)
		},
	}
}

// TargetPath turns the tail of a fleet route into a path on the node.
//
// It is the only place a client influences what the hub asks for, so it is
// resolved and then checked rather than concatenated: a path that cleans its way
// out of /v1 is refused, not forwarded.
func TargetPath(tail string) (string, bool) {
	resolved := path.Clean("/v1/" + strings.TrimPrefix(tail, "/"))

	if resolved != "/v1" && !strings.HasPrefix(resolved, "/v1/") {
		return "", false
	}

	return resolved, true
}
