package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/trnahnh/idemio/internal/correlation"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}
}

type Verdict struct {
	Executions int
	Result     json.RawMessage
}

// The path is declared per resource_type in the manifest. A single hard-coded path would
// make that declaration decorative and silently wrong for the first integration that needs
// a different one.
func (c *Client) Executions(ctx context.Context, path, agentID, key string) (Verdict, error) {
	query := url.Values{}
	query.Set("correlation_id", correlation.ID(agentID, key))
	target := c.baseURL + path + "?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Verdict{}, fmt.Errorf("build probe request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("probe downstream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("probe returned %s", resp.Status)
	}

	var decoded struct {
		Count      int               `json:"count"`
		Executions []json.RawMessage `json:"executions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Verdict{}, fmt.Errorf("decode probe response: %w", err)
	}

	verdict := Verdict{Executions: decoded.Count}
	if len(decoded.Executions) > 0 {
		verdict.Result = decoded.Executions[len(decoded.Executions)-1]
	}
	return verdict, nil
}

type Outcome int

const (
	Unknown Outcome = iota
	Executed
	NotExecuted
)

func (o Outcome) String() string {
	switch o {
	case Executed:
		return "executed"
	case NotExecuted:
		return "not_executed"
	default:
		return "unknown"
	}
}

func (c *Client) Probe(ctx context.Context, path, agentID, key string) (Outcome, json.RawMessage, error) {
	verdict, err := c.Executions(ctx, path, agentID, key)
	if err != nil {
		return Unknown, nil, err
	}
	if verdict.Executions == 0 {
		return NotExecuted, nil, nil
	}
	return Executed, verdict.Result, nil
}
