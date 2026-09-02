package telemetry_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/trnahnh/idemio/internal/telemetry"
	"github.com/trnahnh/idemio/internal/testdb"
)

const metricsDoc = "../../docs/METRICS.md"

var metricName = regexp.MustCompile(`idemio_[a-z0-9_]+`)

func documented(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(metricsDoc)
	if err != nil {
		t.Fatalf("read %s: %v", metricsDoc, err)
	}

	var names []string
	for _, match := range metricName.FindAllString(string(raw), -1) {
		if !slices.Contains(names, match) {
			names = append(names, match)
		}
	}
	slices.Sort(names)
	return names
}

func TestDocumentedMetricsMatchTheRegistry(t *testing.T) {
	pool := testdb.New(t)
	exported := telemetry.New(pool, 65536, nil).Names()
	inDoc := documented(t)

	for _, name := range exported {
		if !slices.Contains(inDoc, name) {
			t.Errorf("%s is exported but absent from %s: an operator wiring an alert "+
				"cannot discover it", name, metricsDoc)
		}
	}
	for _, name := range inDoc {
		if !slices.Contains(exported, name) {
			t.Errorf("%s is documented in %s but not exported: an alert written against it "+
				"would never fire", name, metricsDoc)
		}
	}
}

// SYSTEM_DESIGN explains why each signal exists; METRICS.md owns what it is called. The same
// fact in two files is a bug, so the names must appear in exactly one of them.
func TestSystemDesignDoesNotRestateMetricNames(t *testing.T) {
	raw, err := os.ReadFile("../../docs/SYSTEM_DESIGN.md")
	if err != nil {
		t.Fatalf("read SYSTEM_DESIGN.md: %v", err)
	}

	if found := metricName.FindAllString(string(raw), -1); len(found) > 0 {
		t.Errorf("SYSTEM_DESIGN.md names metrics %v; METRICS.md owns those names", found)
	}
}

func TestEveryDocumentedMetricCarriesAnAlert(t *testing.T) {
	raw, err := os.ReadFile(metricsDoc)
	if err != nil {
		t.Fatalf("read %s: %v", metricsDoc, err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "| `idemio_") {
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) < 6 {
			t.Errorf("row is missing columns: %s", line)
			continue
		}
		if strings.TrimSpace(columns[4]) == "" {
			t.Errorf("%s has no alert or disposition recorded; a metric nobody acts on "+
				"is not observability", strings.TrimSpace(columns[1]))
		}
	}
}
