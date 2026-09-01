package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/api"
	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/downstream"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/telemetry"
	"github.com/trnahnh/idemio/internal/testdb"
)

const (
	agentID    = "agent-checkout-flow"
	otherAgent = "agent-dunning"
	keyA       = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	keyB       = "a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	resourceID = "inv_8842"
)

type harness struct {
	server *httptest.Server
	fake   *faketest.Fake
	pool   *pgxpool.Pool
}

func newHarness(t *testing.T, downstreamTimeout time.Duration) *harness {
	t.Helper()

	return newHarnessWith(t, downstreamTimeout, 0)
}

func newHarnessWith(t *testing.T, downstreamTimeout, pendingWait time.Duration) *harness {
	t.Helper()

	pool := testdb.New(t)
	fake := faketest.Start(t)

	cfg := config.Config{
		AuthMode:                 config.AuthModeTrustedHeader,
		DownstreamBaseURL:        fake.DataURL,
		DownstreamConnectTimeout: 300 * time.Millisecond,
		DownstreamTimeout:        downstreamTimeout,
		ClaimPendingWait:         pendingWait,
		ReconcileStaleAfter:      10 * time.Second,
		ReconcileInterval:        time.Second,
		PayloadBytes:             1024,
		ResultInlineBytes:        65536,
		OutcomeWriteAttempts:     3,
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := downstream.New(fake.DataURL, cfg.DownstreamConnectTimeout, cfg.DownstreamTimeout)
	metrics := telemetry.New(pool, cfg.ResultInlineBytes, logger)
	server := httptest.NewServer(api.New(cfg, pool, client, metrics, logger).Routes())
	t.Cleanup(server.Close)

	return &harness{server: server, fake: fake, pool: pool}
}

func (h *harness) write(t *testing.T, agent, key, payload string) *http.Response {
	t.Helper()

	body := `{"agent_id":"` + agent + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":` + payload + `}`

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/writes", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set(api.HeaderAgentID, agent)
	req.Header.Set(api.HeaderRole, api.RoleAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	return resp
}

func (h *harness) read(t *testing.T, agent, key string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/writes/"+key, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(api.HeaderAgentID, agent)
	req.Header.Set(api.HeaderRole, api.RoleAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp
}

func (h *harness) executions(t *testing.T, agent, key string) int {
	t.Helper()
	return len(h.fake.Executions(t, correlation.ID(agent, key)))
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func TestNewWriteExecutesExactlyOnce(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["status"] != "done" || body["replayed"] != false {
		t.Errorf("body = %v", body)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestReplayReturnsTheStoredResultWithoutReExecuting(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	first := decode(t, h.write(t, agentID, keyA, `{"amount_cents":4200}`))

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", resp.StatusCode)
	}
	second := decode(t, resp)
	if second["replayed"] != true {
		t.Errorf("replayed = %v, want true", second["replayed"])
	}
	if !sameJSON(t, first["result"], second["result"]) {
		t.Errorf("replay returned a different result:\n first: %v\nsecond: %v",
			first["result"], second["result"])
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestBusinessFailureIs201AndReplaysIdentically(t *testing.T) {
	h := newHarness(t, 3*time.Second)
	h.fake.Script(t, resourceID, "business-failure")

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a downstream that answered no still answered", resp.StatusCode)
	}
	first := decode(t, resp)
	if first["status"] != "done" {
		t.Errorf("status = %v, want done", first["status"])
	}

	replay := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", replay.StatusCode)
	}
	second := decode(t, replay)
	if !sameJSON(t, first["result"], second["result"]) {
		t.Errorf("business failure did not replay identically")
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestReserializedRetryReplaysRatherThanRejecting(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	if resp := h.write(t, agentID, keyA, `{"amount_cents":4200,"currency":"EUR"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp.StatusCode)
	}

	resp := h.write(t, agentID, keyA, "{\n  \"currency\" : \"EUR\",\n  \"amount_cents\" : 4200.0\n}")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-serialized retry status = %d, want 200: %v", resp.StatusCode, decode(t, resp))
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestDifferentBodyUnderTheSameKeyIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	resp := h.write(t, agentID, keyA, `{"amount_cents":9900}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if body := decode(t, resp); body["reason"] != "request_hash_mismatch" {
		t.Errorf("reason = %v", body["reason"])
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestConcurrentDuplicateKeysExecuteOnceDownstream(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	const callers = 24
	statuses := make([]int, callers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range callers {
		done.Go(func() {
			start.Wait()
			resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		})
	}
	start.Done()
	done.Wait()

	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times for one key, want 1", got)
	}

	created := 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusOK, http.StatusAccepted:
		default:
			t.Errorf("unexpected status %d", status)
		}
	}
	if created != 1 {
		t.Errorf("%d callers received 201, want 1", created)
	}
}

func TestRefusedDownstreamIs503AndTheKeyIsReclaimable(t *testing.T) {
	h := newHarness(t, 2*time.Second)
	h.fake.SetListener(t, false)

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := decode(t, resp); body["retryable"] != true {
		t.Errorf("retryable = %v, want true", body["retryable"])
	}
	if got := h.executions(t, agentID, keyA); got != 0 {
		t.Fatalf("downstream executed %d times, want 0", got)
	}

	h.fake.SetListener(t, true)

	retry := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if retry.StatusCode != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201: a failed key must be reclaimable", retry.StatusCode)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times after retry, want 1", got)
	}
}

func TestTimeoutIs502AndTheKeyIsNeverRetried(t *testing.T) {
	h := newHarness(t, 700*time.Millisecond)
	h.fake.Script(t, resourceID, "hang")

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["status"] != "indeterminate" || body["retryable"] != false {
		t.Errorf("body = %v", body)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1: the write did happen", got)
	}

	retry := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if retry.StatusCode != http.StatusBadGateway {
		t.Fatalf("retry status = %d, want 502", retry.StatusCode)
	}
	retry.Body.Close()
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times after retry, want 1: indeterminate is terminal", got)
	}
}

func TestAgentIdMismatchIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	body := `{"agent_id":"` + otherAgent + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":{"amount_cents":4200}}`

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/writes", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Idempotency-Key", keyA)
	req.Header.Set(api.HeaderAgentID, agentID)
	req.Header.Set(api.HeaderRole, api.RoleAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMissingIdentityIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	resp, err := http.Post(h.server.URL+"/v1/writes", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestUnknownOperationIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	body := `{"agent_id":"` + agentID + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"delete_everything","payload":{"amount_cents":4200}}`

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/writes", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", keyA)
	req.Header.Set(api.HeaderAgentID, agentID)
	req.Header.Set(api.HeaderRole, api.RoleAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if got := h.executions(t, agentID, keyA); got != 0 {
		t.Fatalf("downstream executed %d times for an unknown operation, want 0", got)
	}
}

func TestOversizedPayloadIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	resp := h.write(t, agentID, keyA, `{"blob":"`+strings.Repeat("x", 4096)+`"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if got := h.executions(t, agentID, keyA); got != 0 {
		t.Fatalf("downstream executed %d times for an oversized payload, want 0", got)
	}
}

func TestMalformedIdempotencyKeyIsRejected(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	resp := h.write(t, agentID, "not-a-uuid", `{"amount_cents":4200}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestReadIsScopedToTheOwningAgent(t *testing.T) {
	h := newHarness(t, 3*time.Second)
	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	owner := h.read(t, agentID, keyA)
	if owner.StatusCode != http.StatusOK {
		t.Fatalf("owner read status = %d, want 200", owner.StatusCode)
	}
	if body := decode(t, owner); body["status"] != "done" {
		t.Errorf("status = %v, want done", body["status"])
	}

	stranger := h.read(t, otherAgent, keyA)
	defer stranger.Body.Close()
	if stranger.StatusCode != http.StatusNotFound {
		t.Fatalf("other agent read status = %d, want 404: 403 would confirm the key exists",
			stranger.StatusCode)
	}
}

func TestUnknownKeyReadsAsNotFound(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	resp := h.read(t, agentID, keyB)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()

	left, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal left: %v", err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal right: %v", err)
	}
	return bytes.Equal(left, right)
}

func (h *harness) metrics(t *testing.T) string {
	t.Helper()

	resp, err := http.Get(h.server.URL + "/metrics")
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

func TestIndeterminateKeysAreExported(t *testing.T) {
	h := newHarness(t, 700*time.Millisecond)
	h.fake.Script(t, resourceID, "hang")

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	scraped := h.metrics(t)
	for _, want := range []string{
		"idemio_indeterminate_keys 1",
		`idemio_writes_total{status="indeterminate"} 1`,
		`idemio_responses_total{code="502"} 1`,
	} {
		if !strings.Contains(scraped, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}

func TestClaimCollisionsAndReplaysAreCounted(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()
	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	scraped := h.metrics(t)
	for _, want := range []string{
		"idemio_claim_collisions_total 1",
		"idemio_replays_total 1",
		"idemio_pending_keys 0",
	} {
		if !strings.Contains(scraped, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}

func TestKeyCasingDoesNotSplitTheCorrelationId(t *testing.T) {
	h := newHarness(t, 3*time.Second)

	upper := strings.ToUpper(keyA)
	h.write(t, agentID, upper, `{"amount_cents":4200}`).Body.Close()

	var stored string
	err := h.pool.QueryRow(context.Background(),
		"SELECT idempotency_key::text FROM idempotency_keys").Scan(&stored)
	if err != nil {
		t.Fatalf("read stored key: %v", err)
	}

	if got := len(h.fake.Executions(t, correlation.ID(agentID, stored))); got != 1 {
		t.Fatalf("the reconciler would find %d executions under the stored key, want 1: "+
			"a probe that finds none marks the key failed, which is re-claimable, "+
			"which executes the write a second time", got)
	}
}

func (h *harness) writeAndHangUp(t *testing.T, agent, key, payload string, after time.Duration) {
	t.Helper()

	body := `{"agent_id":"` + agent + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":` + payload + `}`

	ctx, cancel := context.WithTimeout(context.Background(), after)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/writes",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set(api.HeaderAgentID, agent)
	req.Header.Set(api.HeaderRole, api.RoleAgent)

	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the client was expected to hang up before the response")
	}
}

func (h *harness) awaitTerminal(t *testing.T, agent, key string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := h.pool.QueryRow(context.Background(),
			"SELECT status::text FROM idempotency_keys WHERE agent_id = $1 AND idempotency_key = $2::uuid",
			agent, key).Scan(&status)
		if err == nil && status != "pending" {
			return status
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("key never reached a terminal status")
	return ""
}

// A client hanging up must not turn a healthy write into an indeterminate record, because
// indeterminate is terminal, needs a human, and pages.
func TestClientDisconnectDoesNotAbandonTheWrite(t *testing.T) {
	h := newHarness(t, 3*time.Second)
	h.fake.Script(t, resourceID, "slow")

	h.writeAndHangUp(t, agentID, keyA, `{"amount_cents":4200}`, 100*time.Millisecond)

	if status := h.awaitTerminal(t, agentID, keyA); status != "done" {
		t.Fatalf("status = %s, want done", status)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestPendingWaitLetsTheRaceLoserSeeTheResult(t *testing.T) {
	h := newHarnessWith(t, 3*time.Second, 2*time.Second)
	h.fake.Script(t, resourceID, "slow")

	statuses := make([]int, 2)
	var group sync.WaitGroup
	for i := range statuses {
		group.Go(func() {
			resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		})
	}
	group.Wait()

	created, replayed := 0, 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		default:
			t.Errorf("status = %d, want 201 or 200: the loser should have waited", status)
		}
	}
	if created != 1 || replayed != 1 {
		t.Errorf("created=%d replayed=%d, want 1 and 1", created, replayed)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("downstream executed %d times, want 1", got)
	}
}

func TestZeroPendingWaitReturnsAcceptedImmediately(t *testing.T) {
	h := newHarness(t, 3*time.Second)
	h.fake.Script(t, resourceID, "slow")

	statuses := make([]int, 2)
	var group sync.WaitGroup
	for i := range statuses {
		group.Go(func() {
			resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		})
	}
	group.Wait()

	accepted := 0
	for _, status := range statuses {
		if status == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("statuses = %v, want one 202 when the wait is disabled", statuses)
	}
}
