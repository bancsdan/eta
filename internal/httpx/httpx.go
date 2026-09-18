// Package httpx is the shared HTTP layer for providers: JSON GET/POST with
// a body cap, ETA_DEBUG request logging with secrets redacted, and a common
// mapping of HTTP status codes to transit errors.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/bancsdan/eta/internal/transit"
)

// MaxBody caps response bodies (GTFS-RT feeds can be several MB).
const MaxBody = 32 << 20

type Client struct {
	HTTP   *http.Client
	Name   string
	Header http.Header
	Query  url.Values
	Redact []string            // secrets to mask in debug output
	Signer func(*http.Request) // optional per-request signing (PTV HMAC)
	Debug  bool                // log requests to stderr; defaults from ETA_DEBUG
}

func New(name string, h *http.Client) *Client {
	if h == nil {
		h = http.DefaultClient
	}
	return &Client{HTTP: h, Name: name, Header: http.Header{}, Query: url.Values{}, Debug: os.Getenv("ETA_DEBUG") != ""}
}

func (c *Client) GetJSON(ctx context.Context, u string, out any) error {
	body, err := c.Do(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return err
	}
	return c.decode(u, body, out)
}

func (c *Client) PostJSON(ctx context.Context, u string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	body, err := c.Do(ctx, http.MethodPost, u, b, "application/json")
	if err != nil {
		return err
	}
	return c.decode(u, body, out)
}

// A non-empty "errors" array is an error even when data is present.
func (c *Client) GraphQL(ctx context.Context, u, query string, vars map[string]any, out any) error {
	body := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{query, vars}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.PostJSON(ctx, u, body, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("%s graphql: %s", c.Name, strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return fmt.Errorf("%s graphql: empty response", c.Name)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("%s graphql: bad JSON: %v", c.Name, err)
	}
	return nil
}

// The UTF-8 BOM some feeds (511.org) prefix is stripped.
func (c *Client) GetBytes(ctx context.Context, u string) ([]byte, error) {
	return c.Do(ctx, http.MethodGet, u, nil, "")
}

func (c *Client) Do(ctx context.Context, method, u string, body []byte, contentType string) ([]byte, error) {
	resp, err := c.send(ctx, method, u, body, contentType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return nil, c.netErr(err)
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if err := c.statusErr(resp, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Download streams a GET response body into w without the MaxBody cap, for
// bulk files such as a national GTFS zip.
func (c *Client) Download(ctx context.Context, u string, w io.Writer) error {
	resp, err := c.send(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return c.statusErr(resp, b)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return c.netErr(err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, u string, body []byte, contentType string) (*http.Response, error) {
	if len(c.Query) > 0 {
		pu, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		q := pu.Query()
		for k, vs := range c.Query {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		pu.RawQuery = q.Encode()
		u = pu.String()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "eta-cli (github.com/bancsdan/eta)")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	if c.Signer != nil {
		c.Signer(req)
	}
	if c.Debug {
		fmt.Fprintln(os.Stderr, method, c.redact(req.URL.String()))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.netErr(err)
	}
	if c.Debug {
		fmt.Fprintln(os.Stderr, "   ", resp.Status)
	}
	return resp, nil
}

func (c *Client) statusErr(resp *http.Response, b []byte) error {
	path := resp.Request.URL.Path
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s %s: %w (HTTP %d)", c.Name, path, transit.ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w (HTTP 404): %s", c.Name, path, transit.ErrNotFound, Trim(b))
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%s %s: %w (HTTP 429)", c.Name, path, transit.ErrRateLimited)
	default:
		return fmt.Errorf("%s %s: HTTP %d: %s", c.Name, path, resp.StatusCode, Trim(b))
	}
}

func (c *Client) decode(u string, body []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		path := u
		if pu, perr := url.Parse(u); perr == nil {
			path = pu.Path
		}
		return fmt.Errorf("%s %s: bad JSON: %v", c.Name, path, err)
	}
	return nil
}

func (c *Client) redact(s string) string {
	for _, sec := range c.Redact {
		if sec == "" {
			continue
		}
		s = strings.ReplaceAll(s, url.QueryEscape(sec), "***")
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

func (c *Client) netErr(err error) error {
	var ue *url.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ue) && ue.Timeout()) {
		return fmt.Errorf("%s did not answer in time (the service may be down or overloaded; try again in a minute)", c.Name)
	}
	return fmt.Errorf("%s: network error: %v", c.Name, err)
}

func Trim(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func Join(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func URL(base, path string, q url.Values) string {
	u := Join(base, path)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}
