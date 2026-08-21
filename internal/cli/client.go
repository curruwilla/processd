package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/curruwilla/processd/internal/setup"
)

// client is a thin wrapper over the REST API used by every client command.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient() *client {
	return &client{
		baseURL: viper.GetString("server"),
		token:   resolveToken(),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// resolveToken prefers what the operator passed explicitly, and falls back to
// the token file written by "processd setup". Without that fallback every local
// command would need the secret repeated on the command line, which is how a
// node ends up with several tokens and an unauthorized reply.
func resolveToken() string {
	if token := viper.GetString("token"); token != "" {
		return token
	}

	// An unreadable file is not worth an error here: the request goes out
	// without a token and the daemon answers with the usual 401.
	token, err := setup.ReadToken(viper.GetString("config"))
	if err != nil {
		return ""
	}

	return token
}

// do performs a request and decodes a JSON response into out, if given.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}

		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", path, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		return apiFailure(resp)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response of %s: %w", path, err)
	}

	return nil
}

// maxEventBytes bounds one Server-Sent Event, so a process writing one endless
// line cannot exhaust the client's memory.
const maxEventBytes = 1 << 20

// stream consumes a Server-Sent Events response, handing every complete event
// to fn. It returns when the daemon ends the stream, and reports a cancelled
// context as a clean stop: that is the user pressing Ctrl-C.
func (c *client) stream(ctx context.Context, path string, fn func(event, data string) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Accept", "text/event-stream")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// A followed attempt may run for hours, so the request timeout of the
	// ordinary calls would cut the stream off mid-execution.
	streaming := &http.Client{Transport: c.http.Transport}

	resp, err := streaming.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("calling %s: %w", path, err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		return apiFailure(resp)
	}

	if err := readEvents(resp.Body, fn); err != nil {
		if ctx.Err() != nil {
			return nil
		}

		return err
	}

	return nil
}

// readEvents parses the event stream framing: fields until a blank line, and
// comment lines, which are the heartbeat and carry nothing.
func readEvents(body io.Reader, fn func(event, data string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventBytes)

	var event, data string

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if data != "" {
				if err := fn(event, data); err != nil {
					return err
				}
			}

			event, data = "", ""
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading event stream: %w", err)
	}

	return nil
}

// apiError is a failure the daemon reported through the error contract of
// docs/SPEC.md §6.2. It keeps the machine-readable code, so a command can react
// to one specific failure instead of matching on message text.
type apiError struct {
	code    string
	message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

// hasCode reports whether err is an API failure carrying the given code.
func hasCode(err error, code string) bool {
	var failure *apiError

	return errors.As(err, &failure) && failure.code == code
}

// apiFailure turns an error response into the failure the API reported.
func apiFailure(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error.Code == "" {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}

	return &apiError{code: body.Error.Code, message: body.Error.Message}
}

// query builds a path with an encoded query string.
func query(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}

	return path + "?" + values.Encode()
}
