package pooled_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/api"
	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/downstream"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/fixtures"
	"github.com/trnahnh/idemio/internal/manifest"
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

// Migrations run on the direct connection because they hold a session-scoped advisory lock;
// everything the request path does is transaction-scoped and goes through the pooler.
func newHarness(t *testing.T) *harness {
	t.Helper()

	_, direct := testdb.NewWithURL(t)
	pool := testdb.Pooled(t, direct)

	fake := faketest.Start(t)
	manifestDir := fixtures.ManifestDir(t, fixtures.Enforce)

	cfg := config.Config{
		AuthMode:                 config.AuthModeTrustedHeader,
		DownstreamBaseURL:        fake.DataURL,
		DownstreamConnectTimeout: 300 * time.Millisecond,
		DownstreamTimeout:        5 * time.Second,
		ReconcileStaleAfter:      10 * time.Second,
		ReconcileInterval:        time.Second,
		PayloadBytes:             4096,
		ResultInlineBytes:        65536,
		OutcomeWriteAttempts:     3,
		ManifestDir:              manifestDir,
		ManifestReloadInterval:   time.Hour,
		ConflictLockTimeout:      5 * time.Second,
		ReadMaxSpan:              31 * 24 * time.Hour,
	}

	manifests, err := manifest.NewStore(manifestDir)
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := downstream.New(fake.DataURL, cfg.DownstreamConnectTimeout, cfg.DownstreamTimeout)
	metrics := telemetry.New(pool, cfg, logger)
	server := httptest.NewServer(api.New(cfg, pool, client, manifests, metrics, logger).Routes())
	t.Cleanup(server.Close)

	return &harness{server: server, fake: fake, pool: pool}
}

func (h *harness) write(t *testing.T, agent, key, operation, payload string) *http.Response {
	t.Helper()

	body := `{"agent_id":"` + agent + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"` + operation + `","payload":` + payload + `}`

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

func TestTheWritePathWorksThroughThePooler(t *testing.T) {
	h := newHarness(t)

	resp := h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := len(h.fake.Executions(t, correlation.ID(agentID, keyA))); got != 1 {
		t.Fatalf("executions = %d, want 1", got)
	}
}

func TestReplayWorksThroughThePooler(t *testing.T) {
	h := newHarness(t)

	first := h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`)
	first.Body.Close()

	replay := h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`)
	defer replay.Body.Close()

	if replay.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", replay.StatusCode)
	}
	if got := len(h.fake.Executions(t, correlation.ID(agentID, keyA))); got != 1 {
		t.Fatalf("executions = %d, want 1: the pooler let a replay execute again", got)
	}
}

// pg_advisory_xact_lock is transaction-scoped, which is the whole reason conflict detection
// is safe under transaction pooling where a session lock would not be.
func TestConflictDetectionWorksThroughThePooler(t *testing.T) {
	h := newHarness(t)

	first := h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`)
	first.Body.Close()

	second := h.write(t, otherAgent, keyB, "add_line_item", `{"sku":"x"}`)
	defer second.Body.Close()

	if second.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", second.StatusCode)
	}
	if got := len(h.fake.Executions(t, correlation.ID(otherAgent, keyB))); got != 0 {
		t.Fatalf("the rejected write executed %d times, want 0", got)
	}
}

// The lock has to hold across pooled connections, or concurrent claims would both proceed.
func TestConcurrentClaimsSerializeThroughThePooler(t *testing.T) {
	h := newHarness(t)

	const racers = 8
	statuses := make([]int, racers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			resp := h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`)
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	start.Done()
	done.Wait()

	var created int
	for _, status := range statuses {
		if status == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d racers were told they created the write, want 1: %v", created, statuses)
	}
	if got := len(h.fake.Executions(t, correlation.ID(agentID, keyA))); got != 1 {
		t.Fatalf("executions = %d, want 1: pooling broke the at-most-once guarantee", got)
	}
}

func TestReadEndpointsWorkThroughThePooler(t *testing.T) {
	h := newHarness(t)
	h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`).Body.Close()

	query := url.Values{"since": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
	target := h.server.URL + "/v1/resources/invoice/" + resourceID + "/intents?" + query.Encode()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(api.HeaderAgentID, "operator-jo")
	req.Header.Set(api.HeaderRole, api.RoleOperator)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read intents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Intents []struct {
			IntentID string `json:"intent_id"`
		} `json:"intents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(body.Intents))
	}
}

// A read that must be audited runs the SELECT and the audit INSERT in one transaction,
// which is exactly the unit transaction-mode pooling preserves.
func TestAnAuditedReadCommitsAtomicallyThroughThePooler(t *testing.T) {
	h := newHarness(t)
	h.write(t, agentID, keyA, "create_charge", `{"amount_cents":4200}`).Body.Close()

	query := url.Values{
		"since":   {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"include": {"payload"},
	}
	target := h.server.URL + "/v1/resources/invoice/" + resourceID + "/intents?" + query.Encode()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(api.HeaderAgentID, "investigator-sam")
	req.Header.Set(api.HeaderRole, api.RoleInvestigator)
	req.Header.Set("X-Idemio-Reason", "incident 4412")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read intents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var audited int
	if err := h.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM payload_access_audit").Scan(&audited); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audited != 1 {
		t.Fatalf("audit rows = %d, want 1", audited)
	}
}
