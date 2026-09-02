package fixtures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/trnahnh/idemio/internal/manifest"
)

// Tests run against the manifests that ship, patched rather than re-declared, so a change
// to the real declaration cannot pass a test that copied it.
func repoManifests(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the fixtures package")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", "manifests")
}

type Patch func(map[string]any)

func Enforce(document map[string]any) { document["enforce"] = true }

func Window(ms int) Patch {
	return func(document map[string]any) { document["conflict_window_ms"] = ms }
}

func ManifestDir(t *testing.T, patches ...Patch) string {
	t.Helper()

	source := repoManifests(t)
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}

	target := t.TempDir()
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}

		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		for _, patch := range patches {
			patch(document)
		}

		patched, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			t.Fatalf("encode %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), patched, 0o644); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return target
}

func ManifestStore(t *testing.T, patches ...Patch) *manifest.Store {
	t.Helper()

	store, err := manifest.NewStore(ManifestDir(t, patches...))
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	return store
}
