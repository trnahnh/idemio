package canonical_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trnahnh/idemio/internal/canonical"
)

func TestRFC8785Vectors(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "rfc8785", "input", "*.json"))
	if err != nil {
		t.Fatalf("glob vectors: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatal("no RFC 8785 vectors found")
	}

	for _, input := range inputs {
		name := strings.TrimSuffix(filepath.Base(input), ".json")
		expectedPath := filepath.Join("testdata", "rfc8785", "outhex", name+".txt")

		expectedHex, err := os.ReadFile(expectedPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", expectedPath, err)
		}

		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(input)
			if err != nil {
				t.Fatalf("read %s: %v", input, err)
			}

			want, err := hex.DecodeString(strings.Join(strings.Fields(string(expectedHex)), ""))
			if err != nil {
				t.Fatalf("decode expected hex: %v", err)
			}

			got, err := canonical.Canonicalize(raw)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("canonical form mismatch\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func hashOf(t *testing.T, payload string) string {
	t.Helper()

	digest, err := canonical.Hash(canonical.Request{
		AgentID:      "agent-checkout-flow",
		Operation:    "charge",
		Payload:      json.RawMessage(payload),
		ResourceID:   "order-1",
		ResourceType: "order",
	})
	if err != nil {
		t.Fatalf("hash %s: %v", payload, err)
	}
	return digest
}

// ROADMAP exit criterion 3: a re-serialized retry must replay, not return 422.
func TestReserializedRequestsHashIdentically(t *testing.T) {
	equivalent := []string{
		`{"amount_cents":4200,"currency":"EUR"}`,
		`{"amount_cents":4200.0,"currency":"EUR"}`,
		`{"amount_cents":4.2e3,"currency":"EUR"}`,
		`{"currency":"EUR","amount_cents":4200}`,
		"{\n  \"currency\" : \"EUR\",\n  \"amount_cents\" : 4200\n}",
	}

	want := hashOf(t, equivalent[0])
	for _, payload := range equivalent[1:] {
		if got := hashOf(t, payload); got != want {
			t.Errorf("payload %s hashed to %s, want %s", payload, got, want)
		}
	}
}

func TestDifferentPayloadsHashDifferently(t *testing.T) {
	if hashOf(t, `{"amount_cents":4200}`) == hashOf(t, `{"amount_cents":4201}`) {
		t.Error("distinct payloads produced the same hash")
	}
}

func TestHashCarriesVersionPrefix(t *testing.T) {
	digest := hashOf(t, `{"amount_cents":4200}`)
	if !strings.HasPrefix(digest, canonical.Prefix) {
		t.Errorf("digest %s lacks prefix %s", digest, canonical.Prefix)
	}
	if len(digest) != len(canonical.Prefix)+64 {
		t.Errorf("digest %s is not a hex sha256", digest)
	}
}

func TestFieldsAreDistinguished(t *testing.T) {
	base := canonical.Request{
		AgentID:      "agent-a",
		Operation:    "charge",
		Payload:      json.RawMessage(`{"amount_cents":4200}`),
		ResourceID:   "order-1",
		ResourceType: "order",
	}
	original, err := canonical.Hash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}

	changed := base
	changed.AgentID = "agent-b"
	other, err := canonical.Hash(changed)
	if err != nil {
		t.Fatalf("hash changed: %v", err)
	}
	if original == other {
		t.Error("agent_id does not affect the hash")
	}
}

func TestUnrepresentableNumbersAreRejected(t *testing.T) {
	for _, payload := range []string{
		`{"amount_cents":9007199254740993}`,
		`{"amount_cents":-9007199254740993}`,
		`{"amount_cents":1e400}`,
		`{"amount_cents":NaN}`,
		`{"amount_cents":Infinity}`,
		`{"nested":[{"deep":90071992547409931}]}`,
	} {
		_, err := canonical.Hash(canonical.Request{
			AgentID:      "agent-a",
			Operation:    "charge",
			Payload:      json.RawMessage(payload),
			ResourceID:   "order-1",
			ResourceType: "order",
		})
		if err == nil {
			t.Errorf("payload %s was accepted, want rejection", payload)
		}
	}
}
