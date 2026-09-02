//go:build drill

package drill_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	idemioURL       = "http://localhost:8080"
	prometheusURL   = "http://localhost:9091"
	alertsinkURL    = "http://localhost:9098"
	fakeControlURL  = "http://localhost:8082"
	rulesSource     = "../../deploy/alerts.yml"
	rulesDrill      = "../../deploy/alerts.drill.yml"
	settleTimeout   = 3 * time.Minute
	settleInterval  = 2 * time.Second
	scrapeAllowance = 10 * time.Second
)

func key(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random key: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	h := hex.EncodeToString(buf)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// Each run gets its own alert names. Alertmanager keeps an alert active for minutes after
// Prometheus stops sending it, so a reused name lets a previous run's notification satisfy
// this one — which it did, and the drill passed against a deliberately broken expression
// until this was added.
func nonce(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return "Drill" + hex.EncodeToString(buf)
}

// The drill evaluates the production expressions with their patience collapsed. Generating
// the overlay rather than maintaining a copy is what keeps the drill honest: the expression
// under test is byte-for-byte the one that ships, and only `for:` differs.
var (
	forClause  = regexp.MustCompile(`(?m)^(\s*)for:\s*\S+\s*$`)
	alertName  = regexp.MustCompile(`(?m)^(\s*)- alert:\s*(\S+)\s*$`)
	groupName  = regexp.MustCompile(`(?m)^(\s*)- name:\s*(\S+)\s*$`)
	exprClause = regexp.MustCompile(`(?m)^\s*expr:.*$`)
)

func writeDrillRules(t *testing.T, suffix string) {
	t.Helper()

	source, err := os.ReadFile(rulesSource)
	if err != nil {
		t.Fatalf("read %s: %v", rulesSource, err)
	}

	overlay := forClause.ReplaceAllString(string(source), "${1}for: 0s")
	overlay = alertName.ReplaceAllString(overlay, "${1}- alert: ${2}"+suffix)
	overlay = groupName.ReplaceAllString(overlay, "${1}- name: ${2}-"+strings.ToLower(suffix))

	if err := os.WriteFile(rulesDrill, []byte(overlay), 0o644); err != nil {
		t.Fatalf("write %s: %v", rulesDrill, err)
	}
	t.Cleanup(func() {
		os.Remove(rulesDrill)
		reloadPrometheus(t)
	})

	// The whole point is that only the patience changed.
	original := exprClause.FindAllString(string(source), -1)
	generated := exprClause.FindAllString(overlay, -1)
	if len(original) != len(generated) {
		t.Fatalf("overlay has %d expressions, source has %d", len(generated), len(original))
	}
	for i := range original {
		if original[i] != generated[i] {
			t.Fatalf("overlay changed an expression:\n  source:    %s\n  generated: %s",
				strings.TrimSpace(original[i]), strings.TrimSpace(generated[i]))
		}
	}

	reloadPrometheus(t)
}

func reloadPrometheus(t *testing.T) {
	t.Helper()

	resp, err := http.Post(prometheusURL+"/-/reload", "", nil)
	if err != nil {
		t.Fatalf("reload prometheus: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reload prometheus: %s: %s", resp.Status, body)
	}
}

func post(t *testing.T, url string, body any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("post %s: %s: %s", url, resp.Status, payload)
	}
}

type notification struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

func delivered(t *testing.T, name string) bool {
	t.Helper()

	resp, err := http.Get(alertsinkURL + "/received")
	if err != nil {
		t.Fatalf("read alertsink: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Notifications []notification `json:"notifications"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode alertsink: %v", err)
	}

	for _, n := range body.Notifications {
		for _, a := range n.Alerts {
			if a.Labels["alertname"] == name && a.Status == "firing" {
				if a.Annotations["description"] == "" {
					t.Errorf("%s fired with no description; whoever it wakes gets no instructions", name)
				}
				return true
			}
		}
	}
	return false
}

func waitForDelivery(t *testing.T, name string) {
	t.Helper()

	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if delivered(t, name) {
			return
		}
		time.Sleep(settleInterval)
	}
	t.Fatalf("%s never reached the receiver within %s. The rule exists and references live "+
		"metrics, but nothing was paged.", name, settleTimeout)
}

// ROADMAP Phase 0 exit criterion 6. Until this ran, seventeen alert rules had been written,
// conformance-checked against the exported metrics, and never once observed to fire.
func TestIndeterminateAlertReachesAReceiver(t *testing.T) {
	suffix := nonce(t)
	post(t, alertsinkURL+"/reset", nil)
	writeDrillRules(t, suffix)

	// A hung downstream past the call budget is the honest way to manufacture the outcome:
	// the write is sent, no answer comes back, and nothing can say whether it landed.
	resource := "inv_drill_" + hex.EncodeToString([]byte{byte(time.Now().Unix())})
	post(t, fakeControlURL+"/control/script", map[string]any{
		"resource_id": resource,
		"behaviors":   []string{"hang"},
	})

	written := key(t)
	body := map[string]any{
		"agent_id":      "agent-drill",
		"resource_type": "invoice",
		"resource_id":   resource,
		"operation":     "create_charge",
		"payload":       map[string]any{"amount_cents": 4200},
	}
	encoded, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, idemioURL+"/v1/writes", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build write: %v", err)
	}
	req.Header.Set("Idempotency-Key", written)
	req.Header.Set("X-Idemio-Agent-Id", "agent-drill")
	req.Header.Set("X-Idemio-Role", "agent")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: the drill did not produce an indeterminate key",
			resp.StatusCode)
	}

	time.Sleep(scrapeAllowance)
	waitForDelivery(t, "IdemioIndeterminateKeys"+suffix)
}

// The delivery pipeline is proven once, by the drill above, and this rule routes through the
// same Alertmanager and the same receiver. What is specific to this rule is that it compares
// two live metrics rather than a threshold copied into the expression — so if either side
// were missing, the comparison would quietly produce no result and the alert would never
// fire, which looks exactly like nothing being wrong.
//
// Manufacturing a genuinely stale pending key would mean either racing the reconciler, which
// resolves them within seconds, or inserting a row behind the system's back. Asserting that
// both sides of the comparison are present and comparable is the honest test.
func TestTheStalePendingComparisonHasBothSides(t *testing.T) {
	for _, metric := range []string{
		"idemio_oldest_pending_age_seconds",
		"idemio_reconcile_stale_after_seconds",
	} {
		if !hasSamples(t, metric) {
			t.Fatalf("%s has no samples; the stale-pending rule compares it against the other "+
				"side and would silently never fire", metric)
		}
	}

	// The threshold side must be the configured value, not zero: a zero would make the
	// comparison true forever and page on the first pending write.
	if value := scalar(t, "idemio_reconcile_stale_after_seconds"); value <= 0 {
		t.Fatalf("reconcile stale_after reports %v; the alert threshold would be meaningless", value)
	}
}

func query(t *testing.T, expr string) []any {
	t.Helper()

	resp, err := http.Get(prometheusURL + "/api/v1/query?query=" + url.QueryEscape(expr))
	if err != nil {
		t.Fatalf("query prometheus: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			Result []any `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode query: %v", err)
	}
	return body.Data.Result
}

func hasSamples(t *testing.T, metric string) bool {
	t.Helper()
	return len(query(t, metric)) > 0
}

func scalar(t *testing.T, metric string) float64 {
	t.Helper()

	result := query(t, metric)
	if len(result) == 0 {
		t.Fatalf("%s has no samples", metric)
	}

	sample, ok := result[0].(map[string]any)
	if !ok {
		t.Fatalf("%s returned an unexpected shape", metric)
	}
	pair, ok := sample["value"].([]any)
	if !ok || len(pair) != 2 {
		t.Fatalf("%s returned no value pair", metric)
	}

	var value float64
	if _, err := fmt.Sscanf(fmt.Sprint(pair[1]), "%g", &value); err != nil {
		t.Fatalf("%s value is not a number: %v", metric, pair[1])
	}
	return value
}

func TestTheDashboardReferencesOnlyExportedMetrics(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "grafana", "dashboards",
		"write-path-budget.json"))
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	names := regexp.MustCompile(`idemio_[a-z0-9_]+`).FindAllString(string(raw), -1)
	if len(names) == 0 {
		t.Fatal("the dashboard references no idemio metrics at all")
	}

	resp, err := http.Get(prometheusURL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("read prometheus metric names: %v", err)
	}
	defer resp.Body.Close()

	var known struct {
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&known); err != nil {
		t.Fatalf("decode metric names: %v", err)
	}

	exported := make(map[string]bool, len(known.Data))
	for _, name := range known.Data {
		exported[name] = true
	}

	for _, name := range names {
		if exported[name] {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_count")
		if !exported[base] && !exported[base+"_bucket"] {
			t.Errorf("the dashboard plots %s, which the running system does not export", name)
		}
	}
}
