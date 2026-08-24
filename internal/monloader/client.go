// Package monloader is monbooru's outbound half of the pair: the HTTP
// client that talks to the operator's monloader instance and nothing else.
//
// It exists as its own package because it is the one place monbooru reaches
// the network on purpose, and because it holds no HTTP-server concern - the
// routes, the status cache and the config reads stay in the web layer, which
// hands this the address and the token as functions so a re-pair or a pause
// takes effect on the next call without a restart.
package monloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// App is the peer's name in pairing records and error text.
const App = "monloader"

// httpClient is the only outbound HTTP client in monbooru's own code, and it
// is only ever pointed at the configured instance.
// Per-call deadlines belong to the request contexts (probes 4-5 s,
// contribution previews 8 s, sends 10 s); the client timeout is only a
// backstop for the callers that pass an unbounded context, and must stay
// above the largest per-call deadline or it aborts a send monloader may
// still commit.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// ErrUnconfigured is what every outbound call answers before any I/O when
// no link is set up.
var ErrUnconfigured = errors.New("monloader is not configured")

// Client issues authed calls to one monloader. Base and Token are read per
// call rather than captured, so a re-pair, a paused link or an address
// change lands without rebuilding anything. Base answers "" when the link is
// paused or unset, which is what makes ErrUnconfigured the first answer.
type Client struct {
	Base  func() string
	Token func() string
}

// Do issues one authed request to a monloader API path and returns the live
// response for the caller to map. A nil body sends no payload and no content
// type.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(c.Base(), "/")
	token := c.Token()
	if base == "" || token == "" {
		return nil, ErrUnconfigured
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(req)
}

// Post sends one JSON body to a monloader API path.
func (c *Client) Post(ctx context.Context, path string, payload map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return c.Do(ctx, http.MethodPost, path, body)
}
