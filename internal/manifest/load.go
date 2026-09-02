package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

type Snapshot struct {
	version string
	byType  map[string]Definition
}

func (s *Snapshot) Version() string { return s.version }

func (s *Snapshot) Lookup(resourceType string) (Definition, bool) {
	definition, ok := s.byType[resourceType]
	return definition, ok
}

func (s *Snapshot) Operation(resourceType, operation string) (Operation, bool) {
	definition, ok := s.byType[resourceType]
	if !ok {
		return Operation{}, false
	}
	declared, ok := definition.Operations[operation]
	return declared, ok
}

func (s *Snapshot) Types() []string {
	types := make([]string, 0, len(s.byType))
	for name := range s.byType {
		types = append(types, name)
	}
	slices.Sort(types)
	return types
}

func Load(dir string) (*Snapshot, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("list manifests in %s: %w", dir, err)
	}
	slices.Sort(paths)

	if len(paths) == 0 {
		return nil, fmt.Errorf("no manifests in %s; a process that declares no resource type "+
			"can serve no write", dir)
	}

	sum := sha256.New()
	byType := make(map[string]Definition, len(paths))
	var problems []string

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		stem := strings.TrimSuffix(filepath.Base(path), ".json")
		sum.Write([]byte(stem))
		sum.Write(raw)

		var doc document
		if err := json.Unmarshal(raw, &doc); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", stem, err))
			continue
		}
		problems = append(problems, doc.validate(stem)...)

		if _, duplicate := byType[doc.ResourceType]; duplicate {
			problems = append(problems, fmt.Sprintf("%s: resource_type %q is declared twice",
				stem, doc.ResourceType))
			continue
		}
		byType[doc.ResourceType] = doc.definition()
	}

	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, fmt.Errorf("manifest: %s", strings.Join(problems, "; "))
	}
	return &Snapshot{version: hex.EncodeToString(sum.Sum(nil))[:16], byType: byType}, nil
}

type Store struct {
	dir     string
	current atomic.Pointer[Snapshot]
}

func NewStore(dir string) (*Store, error) {
	snapshot, err := Load(dir)
	if err != nil {
		return nil, err
	}
	store := &Store{dir: dir}
	store.current.Store(snapshot)
	return store, nil
}

func (s *Store) Current() *Snapshot { return s.current.Load() }

var errUnchanged = errors.New("manifest unchanged")

func (s *Store) reload() (*Snapshot, error) {
	snapshot, err := Load(s.dir)
	if err != nil {
		return nil, err
	}
	if snapshot.Version() == s.Current().Version() {
		return nil, errUnchanged
	}
	s.current.Store(snapshot)
	return snapshot, nil
}

// A running process keeps the last manifest that validated as a whole. Refusing to boot on
// a bad manifest is right; stopping a serving fleet because one arrived later is not
// (ADR-0013).
func (s *Store) Watch(ctx context.Context, interval time.Duration, logger *slog.Logger,
	onActivate func(*Snapshot), onFailure func()) {

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot, err := s.reload()
			if errors.Is(err, errUnchanged) {
				continue
			}
			if err != nil {
				onFailure()
				logger.Error("manifest reload rejected; still serving the previous version",
					"version", s.Current().Version(), "error", err)
				continue
			}
			logger.Info("manifest activated",
				"version", snapshot.Version(), "resource_types", snapshot.Types())
			onActivate(snapshot)
		}
	}
}
