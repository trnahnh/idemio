package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/api"
)

func (h *harness) get(t *testing.T, path, role string, query url.Values, headers map[string]string) *http.Response {
	t.Helper()

	target := h.server.URL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(api.HeaderAgentID, "operator-jo")
	req.Header.Set(api.HeaderRole, role)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return resp
}

func since(d time.Duration) url.Values {
	return url.Values{"since": {time.Now().Add(-d).UTC().Format(time.RFC3339)}}
}

const intentsPath = "/v1/resources/invoice/" + resourceID + "/intents"

func decodeList[T any](t *testing.T, resp *http.Response, field string) ([]T, map[string]any) {
	t.Helper()
	defer resp.Body.Close()

	var envelope map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	var rows []T
	if raw, ok := envelope[field]; ok {
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("decode %s: %v", field, err)
		}
	}

	meta := make(map[string]any)
	for name, raw := range envelope {
		if name == field {
			continue
		}
		var value any
		json.Unmarshal(raw, &value)
		meta[name] = value
	}
	return rows, meta
}

type intentView struct {
	IntentID  string          `json:"intent_id"`
	AgentID   string          `json:"agent_id"`
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
	Redacted  bool            `json:"payload_redacted"`
	EmittedAt time.Time       `json:"emitted_at"`
}

func TestReadEndpointsRequireAnOperatorRole(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	for _, path := range []string{intentsPath, "/v1/conflicts"} {
		resp := h.get(t, path, api.RoleAgent, since(time.Hour), nil)
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s as agent = %d, want 403", path, resp.StatusCode)
		}
	}
}

// ADR-0017: a default window would let a caller believe they searched a period they did not.
func TestTheTimeRangeIsRequiredAndCapped(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	cases := []struct {
		name  string
		query url.Values
	}{
		{"absent", url.Values{}},
		{"unparseable", url.Values{"since": {"yesterday"}}},
		{"wider than the cap", url.Values{
			"since": {time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		}},
		{"inverted", url.Values{
			"since": {time.Now().UTC().Format(time.RFC3339)},
			"until": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		}},
	}

	for _, tc := range cases {
		resp := h.get(t, intentsPath, api.RoleOperator, tc.query, nil)
		resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s range = %d, want 400", tc.name, resp.StatusCode)
		}
	}
}

func TestOperatorsSeeMetadataAndNeverPayloads(t *testing.T) {
	h := newHarness(t, 5*time.Second)
	h.write(t, agentID, keyA, `{"amount_cents":4200,"customer_id":"cus_119"}`).Body.Close()

	resp := h.get(t, intentsPath, api.RoleOperator, since(time.Hour), nil)
	rows, _ := decodeList[intentView](t, resp, "intents")

	if len(rows) != 1 {
		t.Fatalf("intents = %d, want 1", len(rows))
	}
	if rows[0].AgentID != agentID || rows[0].Operation != "create_charge" {
		t.Errorf("metadata is wrong: %+v", rows[0])
	}
	if !rows[0].Redacted || len(rows[0].Payload) != 0 && string(rows[0].Payload) != "null" {
		t.Fatalf("an operator received a payload: %s", rows[0].Payload)
	}
}

func TestPayloadsRequireInvestigatorAndAStatedReason(t *testing.T) {
	h := newHarness(t, 5*time.Second)
	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	query := since(time.Hour)
	query.Set("include", "payload")

	operator := h.get(t, intentsPath, api.RoleOperator, query, nil)
	operator.Body.Close()
	if operator.StatusCode != http.StatusForbidden {
		t.Errorf("operator with include=payload = %d, want 403", operator.StatusCode)
	}

	unexplained := h.get(t, intentsPath, api.RoleInvestigator, query, nil)
	unexplained.Body.Close()
	if unexplained.StatusCode != http.StatusBadRequest {
		t.Errorf("investigator with no reason = %d, want 400", unexplained.StatusCode)
	}
}

func TestAnUnredactedReadReturnsPayloadsAndAuditsItself(t *testing.T) {
	h := newHarness(t, 5*time.Second)
	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	query := since(time.Hour)
	query.Set("include", "payload")

	resp := h.get(t, intentsPath, api.RoleInvestigator, query,
		map[string]string{"X-Idemio-Reason": "incident 4412"})
	rows, _ := decodeList[intentView](t, resp, "intents")

	if len(rows) != 1 {
		t.Fatalf("intents = %d, want 1", len(rows))
	}
	if rows[0].Redacted {
		t.Error("the response is marked redacted despite carrying a payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["amount_cents"] != float64(4200) {
		t.Errorf("payload = %v, want the original body", payload)
	}

	var principal, role, reason string
	var count int
	var ids []string
	err := h.pool.QueryRow(context.Background(),
		`SELECT principal, caller_role, coalesce(stated_reason, ''), record_count, intent_ids::text[]
		   FROM payload_access_audit`).Scan(&principal, &role, &reason, &count, &ids)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if role != api.RoleInvestigator || reason != "incident 4412" || count != 1 {
		t.Errorf("audit row = role %q reason %q count %d", role, reason, count)
	}
	if len(ids) != 1 || ids[0] != rows[0].IntentID {
		t.Errorf("audit recorded %v, want the intent that was read", ids)
	}
}

// The audit row records which records were read, never their contents (ADR-0011).
func TestTheAuditRowHoldsNoPayload(t *testing.T) {
	h := newHarness(t, 5*time.Second)
	h.write(t, agentID, keyA, `{"amount_cents":4200,"secret":"hunter2"}`).Body.Close()

	query := since(time.Hour)
	query.Set("include", "payload")
	h.get(t, intentsPath, api.RoleInvestigator, query,
		map[string]string{"X-Idemio-Reason": "incident 4412"}).Body.Close()

	var dump string
	if err := h.pool.QueryRow(context.Background(),
		"SELECT coalesce(string_agg(payload_access_audit::text, ' '), '') FROM payload_access_audit").
		Scan(&dump); err != nil {
		t.Fatalf("dump audit: %v", err)
	}
	if strings.Contains(dump, "hunter2") {
		t.Fatal("the audit table contains payload content; it is now an unaudited copy of what it protects")
	}
}

func TestKeysetPagingWalksEveryIntentExactlyOnce(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	const writes = 5
	for i := range writes {
		key := fmt.Sprintf("7c9e6679-7425-40de-944b-e07fc1f9%04d", i)
		h.writeOp(t, agentID, key, "invoice", resourceID, "add_line_item",
			fmt.Sprintf(`{"sku":"item-%d"}`, i)).Body.Close()
	}

	seen := make(map[string]int)
	query := since(time.Hour)
	query.Set("limit", "2")

	for page := 0; page < writes+2; page++ {
		resp := h.get(t, intentsPath, api.RoleOperator, query, nil)
		rows, meta := decodeList[intentView](t, resp, "intents")

		for _, row := range rows {
			seen[row.IntentID]++
		}
		if meta["has_more"] != true {
			break
		}
		query.Set("cursor", meta["next_cursor"].(string))
	}

	if len(seen) != writes {
		t.Fatalf("paged over %d distinct intents, want %d", len(seen), writes)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("intent %s appeared %d times across pages", id, count)
		}
	}
}

type conflictView struct {
	ConflictID      string `json:"conflict_id"`
	AgentIDA        string `json:"agent_id_a"`
	AgentIDB        string `json:"agent_id_b"`
	Resolution      string `json:"resolution"`
	ManifestVersion string `json:"manifest_version"`
}

func TestConflictsAreListedWithTheManifestThatJudgedThem(t *testing.T) {
	h := newEnforcingHarness(t)

	h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":4200}`).Body.Close()
	h.writeOp(t, otherAgent, keyB, "invoice", resourceID, "add_line_item", `{"sku":"x"}`).Body.Close()

	resp := h.get(t, "/v1/conflicts", api.RoleOperator, since(time.Hour), nil)
	rows, _ := decodeList[conflictView](t, resp, "conflicts")

	if len(rows) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(rows))
	}
	if rows[0].Resolution != "rejected" {
		t.Errorf("resolution = %s, want rejected", rows[0].Resolution)
	}
	if rows[0].ManifestVersion == "" {
		t.Error("the conflict does not record which manifest judged it")
	}

	filtered := since(time.Hour)
	filtered.Set("resolution", "serialized")
	empty, _ := decodeList[conflictView](t,
		h.get(t, "/v1/conflicts", api.RoleOperator, filtered, nil), "conflicts")
	if len(empty) != 0 {
		t.Errorf("filtering by an absent resolution returned %d rows", len(empty))
	}

	byAgent := since(time.Hour)
	byAgent.Set("agent_id", otherAgent)
	mine, _ := decodeList[conflictView](t,
		h.get(t, "/v1/conflicts", api.RoleOperator, byAgent, nil), "conflicts")
	if len(mine) != 1 {
		t.Errorf("filtering by agent returned %d rows, want 1", len(mine))
	}
}

func TestConflictsCarryNoPayloadToInclude(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	query := since(time.Hour)
	query.Set("include", "payload")

	resp := h.get(t, "/v1/conflicts", api.RoleInvestigator, query,
		map[string]string{"X-Idemio-Reason": "incident 4412"})
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: conflict records hold no payload", resp.StatusCode)
	}
}
