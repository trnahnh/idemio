package manifest_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/fixtures"
	"github.com/trnahnh/idemio/internal/manifest"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const valid = `{
  "resource_type": "invoice",
  "conflict_window_ms": 5000,
  "probe_path": "/probe",
  "operations": {"create_charge": {"class": "create"}},
  "errors": {"definitive": [{"from": 200, "to": 299}]}
}`

func TestTheShippedManifestsLoad(t *testing.T) {
	store := fixtures.ManifestStore(t)

	snapshot := store.Current()
	for _, want := range []string{"invoice", "subscription"} {
		definition, ok := snapshot.Lookup(want)
		if !ok {
			t.Fatalf("%s is not declared", want)
		}
		if definition.ConflictWindow <= 0 || definition.ProbePath == "" {
			t.Errorf("%s is declared but incomplete: %+v", want, definition)
		}
	}
}

// The per-resource_type wiring is only load-bearing if two types actually differ.
func TestTheTwoShippedTypesDeclareDifferentSemantics(t *testing.T) {
	snapshot := fixtures.ManifestStore(t).Current()

	invoice, _ := snapshot.Lookup("invoice")
	subscription, _ := snapshot.Lookup("subscription")

	if invoice.ConflictWindow == subscription.ConflictWindow {
		t.Error("both types share a conflict window; per-type windows are untested")
	}
	if invoice.Errors.IsNotExecuted(429) == subscription.Errors.IsNotExecuted(429) {
		t.Error("both types classify 429 identically; per-type classification is untested")
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		file string
		body string
		want string
	}{
		{"type not matching the file name", "invoice.json",
			strings.Replace(valid, `"invoice"`, `"invoic"`, 1), "the file name is the type"},
		{"unknown operation class", "invoice.json",
			strings.Replace(valid, `"create"`, `"upsert"`, 1), "which is not one of"},
		{"scope on a non-mutate", "invoice.json",
			strings.Replace(valid, `{"class": "create"}`, `{"class": "append", "scope": ["x"]}`, 1),
			"selectors are only consulted between two mutates"},
		{"zero conflict window", "invoice.json",
			strings.Replace(valid, `"conflict_window_ms": 5000`, `"conflict_window_ms": 0`, 1),
			"conflict_window_ms must be positive"},
		{"no probe path", "invoice.json",
			strings.Replace(valid, `"/probe"`, `""`, 1), "is not a path"},
		{"no error classification", "invoice.json",
			strings.Replace(valid, `[{"from": 200, "to": 299}]`, `[]`, 1),
			"declares no error classification"},
		{"overlapping classification", "invoice.json",
			strings.Replace(valid, `"errors": {"definitive": [{"from": 200, "to": 299}]}`,
				`"errors": {"definitive": [{"from": 200, "to": 499}], "not_executed": [{"from": 429, "to": 429}]}`, 1),
			"cannot be both"},
		{"malformed json", "invoice.json", "{", "unexpected end"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.file, tc.body)

			_, err := manifest.Load(dir)
			if err == nil {
				t.Fatal("loaded a manifest that should have been rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAnEmptyDirectoryIsRefused(t *testing.T) {
	if _, err := manifest.Load(t.TempDir()); err == nil {
		t.Fatal("a process declaring no resource type can serve no write, but the manifest loaded")
	}
}

func TestVersionTracksContent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "invoice.json", valid)

	first, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	write(t, dir, "invoice.json", strings.Replace(valid, "5000", "6000", 1))
	second, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if first.Version() == second.Version() {
		t.Fatal("the version did not change with the content; a conflict verdict could not be " +
			"attributed to the rules that produced it")
	}
}

// ADR-0013: a bad manifest arriving under a serving process must not stop it, and must not
// be applied in part.
func TestABadReloadKeepsTheLastGoodManifest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "invoice.json", valid)

	store, err := manifest.NewStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	original := store.Current().Version()

	write(t, dir, "subscription.json", `{"resource_type": "subscription"}`)

	var failures atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	go store.Watch(ctx, 10*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)),
		func(*manifest.Snapshot) { t.Error("an invalid manifest was activated") },
		func() { failures.Add(1) })

	waitFor(t, func() bool { return failures.Load() > 0 })
	cancel()

	if store.Current().Version() != original {
		t.Fatal("the live manifest changed despite failing validation")
	}
	if _, ok := store.Current().Lookup("invoice"); !ok {
		t.Fatal("the previously valid type stopped being served")
	}
}

func TestAValidReloadIsActivated(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "invoice.json", valid)

	store, err := manifest.NewStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	var activated atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go store.Watch(ctx, 10*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)),
		func(*manifest.Snapshot) { activated.Add(1) },
		func() { t.Error("a valid manifest failed to reload") })

	write(t, dir, "invoice.json", strings.Replace(valid, "5000", "7000", 1))
	waitFor(t, func() bool { return activated.Load() > 0 })

	definition, _ := store.Current().Lookup("invoice")
	if definition.ConflictWindow != 7*time.Second {
		t.Fatalf("conflict window = %s, want 7s: the reload did not take effect", definition.ConflictWindow)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
