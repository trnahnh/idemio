package downstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/trnahnh/idemio/internal/correlation"
)

// The correlation header must never be named Idempotency-Key or X-Idempotency-Key:
// net/http treats a POST carrying either as replayable and may re-send it on a reused
// connection, executing the write twice. ADR-0012.
const CorrelationHeader = "X-Idemio-Correlation-Id"

type Disposition int

// The zero value is Indeterminate so an unclassified path fails safe: ADR-0005.
const (
	Indeterminate Disposition = iota
	Done
	Failed
)

func (d Disposition) String() string {
	switch d {
	case Done:
		return "done"
	case Failed:
		return "failed"
	default:
		return "indeterminate"
	}
}

type Outcome struct {
	Disposition Disposition
	Result      json.RawMessage
	Detail      string
}

type Request struct {
	AgentID      string
	Key          string
	ResourceType string
	ResourceID   string
	Operation    string
	Payload      json.RawMessage
}

type Client struct {
	baseURL string
	http    *http.Client
}

// notDialed marks an error raised before a connection existed, which is the only evidence
// that nothing was sent and therefore nothing executed.
type notDialed struct{ err error }

func (e *notDialed) Error() string { return e.err.Error() }
func (e *notDialed) Unwrap() error { return e.err }

func New(baseURL string, connectTimeout, timeout time.Duration) *Client {
	dialer := &net.Dialer{Timeout: connectTimeout}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, &notDialed{err: err}
			}
			return conn, nil
		},
	}

	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Transport: transport, Timeout: timeout},
	}
}

func (c *Client) Execute(ctx context.Context, req Request) Outcome {
	body, err := json.Marshal(map[string]any{
		"resource_type": req.ResourceType,
		"resource_id":   req.ResourceID,
		"operation":     req.Operation,
		"payload":       req.Payload,
	})
	if err != nil {
		return Outcome{Disposition: Failed, Detail: fmt.Sprintf("encode request: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/execute", bytes.NewReader(body))
	if err != nil {
		return Outcome{Disposition: Failed, Detail: fmt.Sprintf("build request: %v", err)}
	}
	// Second guard against the transparent retry described above.
	httpReq.GetBody = nil
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(CorrelationHeader, correlation.ID(req.AgentID, req.Key))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return classifyTransportError(err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return Outcome{Detail: fmt.Sprintf("read response after send: %v", err)}
	}
	return classifyResponse(resp.StatusCode, payload)
}

func classifyTransportError(err error) Outcome {
	var dial *notDialed
	if errors.As(err, &dial) {
		return Outcome{
			Disposition: Failed,
			Detail:      fmt.Sprintf("downstream_unreachable: %v", dial.err),
		}
	}
	return Outcome{Detail: fmt.Sprintf("downstream_timeout_after_send: %v", err)}
}

// A definitive answer is a definitive outcome even when the answer is "no": a business
// failure is done, not failed. Anything a server could have half-applied is indeterminate.
func classifyResponse(status int, payload []byte) Outcome {
	switch {
	case status >= 200 && status < 300:
		return Outcome{Disposition: Done, Result: asJSON(payload)}
	case status >= 400 && status < 500:
		return Outcome{Disposition: Done, Result: asJSON(payload)}
	default:
		return Outcome{
			Result: asJSON(payload),
			Detail: fmt.Sprintf("downstream_status_%d", status),
		}
	}
}

func asJSON(payload []byte) json.RawMessage {
	if json.Valid(payload) {
		return json.RawMessage(payload)
	}
	encoded, err := json.Marshal(map[string]string{"raw": string(payload)})
	if err != nil {
		return json.RawMessage(`{"raw":""}`)
	}
	return encoded
}
