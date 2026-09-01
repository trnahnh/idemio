package downstream_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/downstream"
	"github.com/trnahnh/idemio/internal/faketest"
)

const (
	agentID = "agent-checkout-flow"
	keyA    = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

func request() downstream.Request {
	return downstream.Request{
		AgentID:      agentID,
		Key:          keyA,
		ResourceType: "invoice",
		ResourceID:   "inv_8842",
		Operation:    "create_charge",
		Payload:      json.RawMessage(`{"amount_cents":4200}`),
	}
}

func client(baseURL string, timeout time.Duration) *downstream.Client {
	return downstream.New(baseURL, 500*time.Millisecond, timeout)
}

func TestSuccessIsDone(t *testing.T) {
	fake := faketest.Start(t)

	outcome := client(fake.DataURL, 3*time.Second).Execute(context.Background(), request())
	if outcome.Disposition != downstream.Done {
		t.Fatalf("disposition = %s, want done: %s", outcome.Disposition, outcome.Detail)
	}
}

func TestBusinessFailureIsDoneNotFailed(t *testing.T) {
	fake := faketest.Start(t)
	fake.Script(t, "inv_8842", "business-failure")

	outcome := client(fake.DataURL, 3*time.Second).Execute(context.Background(), request())
	if outcome.Disposition != downstream.Done {
		t.Fatalf("disposition = %s, want done: a downstream that answered 'no' answered",
			outcome.Disposition)
	}
	if len(outcome.Result) == 0 {
		t.Error("business failure stored no result to replay")
	}
}

func TestRefusedConnectionIsFailed(t *testing.T) {
	fake := faketest.Start(t)
	fake.SetListener(t, false)

	outcome := client(fake.DataURL, 3*time.Second).Execute(context.Background(), request())
	if outcome.Disposition != downstream.Failed {
		t.Fatalf("disposition = %s, want failed: a refused connection proves nothing executed",
			outcome.Disposition)
	}
	if executions := fake.Executions(t, ""); len(executions) != 0 {
		t.Fatalf("downstream recorded %d executions, want 0", len(executions))
	}
}

// The case the taxonomy exists for: the write did execute, and we cannot know it.
func TestTimeoutAfterSendIsIndeterminateEvenThoughItExecuted(t *testing.T) {
	fake := faketest.Start(t)
	fake.Script(t, "inv_8842", "hang")

	outcome := client(fake.DataURL, 700*time.Millisecond).Execute(context.Background(), request())
	if outcome.Disposition != downstream.Indeterminate {
		t.Fatalf("disposition = %s, want indeterminate", outcome.Disposition)
	}

	executions := fake.Executions(t, "")
	if len(executions) != 1 {
		t.Fatalf("downstream recorded %d executions, want 1: the write did happen", len(executions))
	}
}

func TestZeroOutcomeIsIndeterminate(t *testing.T) {
	var outcome downstream.Outcome
	if outcome.Disposition != downstream.Indeterminate {
		t.Fatal("the zero Outcome is not indeterminate; an unclassified path would not fail safe")
	}
}

func TestCorrelationHeaderIsNotAnIdempotencyKeyHeader(t *testing.T) {
	for _, forbidden := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		if http.CanonicalHeaderKey(downstream.CorrelationHeader) == forbidden {
			t.Fatalf("correlation header is %q, which makes net/http treat the write as replayable",
				downstream.CorrelationHeader)
		}
	}
}

var crlf = string([]byte{13, 10})

type countingServer struct {
	listener net.Listener
	mu       sync.Mutex
	requests int
}

func startCountingServer(t *testing.T) *countingServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &countingServer{listener: listener}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return server
}

// Answers the first request on a connection and keeps it alive, then reads the second
// request in full and drops the connection without responding. That is the dangerous shape:
// the server received the write and could have applied it, and the client cannot tell.
func (s *countingServer) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	for seen := 0; ; seen++ {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		req.Body.Close()

		s.mu.Lock()
		s.requests++
		s.mu.Unlock()

		if seen > 0 {
			return
		}
		conn.Write([]byte("HTTP/1.1 200 OK" + crlf + "Content-Type: application/json" + crlf +
			"Content-Length: 2" + crlf + crlf + "{}"))
	}
}

func (s *countingServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func TestTransportDoesNotReplayAWriteTheServerAlreadyRead(t *testing.T) {
	server := startCountingServer(t)
	downstreamClient := client("http://"+server.listener.Addr().String(), 3*time.Second)

	first := downstreamClient.Execute(context.Background(), request())
	if first.Disposition != downstream.Done {
		t.Fatalf("first call disposition = %s, want done", first.Disposition)
	}

	second := downstreamClient.Execute(context.Background(), request())
	if second.Disposition == downstream.Done {
		t.Fatal("a call the server never answered reported success")
	}
	if second.Disposition == downstream.Failed {
		t.Fatal("a reused-connection failure was classified failed; it is not provably unexecuted")
	}
	if got := server.count(); got != 2 {
		t.Fatalf("server received %d requests, want 2: net/http replayed the write", got)
	}
}

// Control: the same server, driven by a request shaped the way net/http considers
// replayable. If this does not double-deliver, the test above proves nothing.
func TestTheReplayHazardIsRealForAnIdempotencyKeyHeader(t *testing.T) {
	server := startCountingServer(t)
	target := "http://" + server.listener.Addr().String() + "/v1/execute"
	httpClient := &http.Client{Timeout: 3 * time.Second}

	send := func() {
		req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"amount_cents":4200}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Idempotency-Key", keyA)
		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	send()
	send()

	if got := server.count(); got != 3 {
		t.Fatalf("server received %d requests, want 3: the hazard did not reproduce, so the "+
			"guard test above is not proving anything", got)
	}
}
