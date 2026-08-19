package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/viper"
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
		token:   viper.GetString("token"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
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

// apiFailure turns an error response into the message the API reported.
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

	return fmt.Errorf("%s: %s", body.Error.Code, body.Error.Message)
}

// query builds a path with an encoded query string.
func query(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}

	return path + "?" + values.Encode()
}
