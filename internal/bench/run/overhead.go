package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// overheadClient asks corrallm how much of a stage was corrallm rather than the
// model: admission queueing and cold spawns, which a wall clock cannot separate
// from generation.
//
// A stage is an agent loop of many calls, so the useful question is a SUM over
// the stage's window rather than a per-response header — which is just as well,
// since agentkit's client does not surface response headers to its caller.
type overheadClient struct {
	base  string
	token string
	key   string
	http  *http.Client
}

// newOverheadClient returns nil when the bench cannot ask.
//
// Nil is the normal case for a run against something that is not corrallm, and
// for one with no admin token. It must stay a no-op rather than an error: the
// correction is an improvement to a measurement, not a precondition for taking
// one, and failing the run because it is unavailable would be a regression for
// every existing setup.
func newOverheadClient(cfg Config) *overheadClient {
	key := ""
	if env := cfg.LLM.APIKeyEnv; env != "" {
		key = strings.TrimSpace(os.Getenv(env))
	}
	if key == "" {
		return nil // unkeyed requests cannot be told apart from anyone else's
	}
	tok := ""
	if p := cfg.LLM.AdminTokenFile; p != "" {
		if b, err := os.ReadFile(p); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" && cfg.LLM.AdminTokenEnv != "" {
		tok = strings.TrimSpace(os.Getenv(cfg.LLM.AdminTokenEnv))
	}
	if tok == "" {
		return nil // /api is admin-gated
	}
	return &overheadClient{
		base:  strings.TrimSuffix(strings.TrimRight(cfg.LLM.BaseURL, "/"), "/v1"),
		token: tok,
		key:   key,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Between sums the queue and load time corrallm imposed on this bench's requests
// to one model between two instants.
func (c *overheadClient) Between(ctx context.Context, model string, from, to time.Time) (time.Duration, error) {
	if c == nil {
		return 0, nil
	}
	q := url.Values{}
	q.Set("key", c.key)
	q.Set("served", model)
	q.Set("from", strconv.FormatInt(from.UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(to.UnixMilli(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/overhead?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("overhead: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TotalMS int64 `json:"totalMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return time.Duration(body.TotalMS) * time.Millisecond, nil
}
