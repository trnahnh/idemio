package resultstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/objectstore"
)

const prefix = "results/"

type Objects interface {
	Put(ctx context.Context, name string, body []byte, contentType string) error
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, names ...string) error
}

type Store struct {
	objects   Objects
	inlineCap int64
}

// The concrete client is taken by pointer and only promoted to the interface when it is
// non-nil: a nil *Client assigned to an interface is not a nil interface, and every
// unconfigured deployment would panic on the first oversized result.
func New(objects *objectstore.Client, inlineCap int64) *Store {
	if objects == nil {
		return &Store{inlineCap: inlineCap}
	}
	return NewWith(objects, inlineCap)
}

func NewWith(objects Objects, inlineCap int64) *Store {
	return &Store{objects: objects, inlineCap: inlineCap}
}

func Ref(agentID, key string) string {
	return prefix + correlation.ID(agentID, key) + ".json"
}

func IsRef(value string) bool {
	return strings.HasPrefix(value, prefix)
}

type Placement struct {
	Inline    json.RawMessage
	Ref       string
	Offloaded bool
	FellBack  bool
}

// The cap is a performance guard, not a correctness one. If the object store will not take
// the result, storing it inline over the cap is strictly better than losing it, so the
// failure degrades to the behaviour that existed before offload rather than to data loss.
func (s *Store) Place(ctx context.Context, agentID, key string, result json.RawMessage) Placement {
	if len(result) == 0 || s == nil || s.objects == nil || int64(len(result)) <= s.inlineCap {
		return Placement{Inline: result}
	}

	ref := Ref(agentID, key)
	if err := s.objects.Put(ctx, ref, result, "application/json"); err != nil {
		return Placement{Inline: result, FellBack: true}
	}
	return Placement{Ref: ref, Offloaded: true}
}

// Resolve is the read half. A result that was offloaded and cannot be fetched is not a
// missing write: the key is terminal and the write definitely happened, which is why the
// caller reports a layer failure rather than anything from the not-executed family.
func (s *Store) Resolve(ctx context.Context, inline json.RawMessage, ref string) (json.RawMessage, error) {
	if ref == "" {
		return inline, nil
	}
	if s == nil || s.objects == nil {
		return nil, fmt.Errorf("result %s is offloaded but no object storage is configured", ref)
	}

	body, err := s.objects.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

func (s *Store) Delete(ctx context.Context, refs []string) error {
	if len(refs) == 0 || s == nil || s.objects == nil {
		return nil
	}
	return s.objects.Delete(ctx, refs...)
}
