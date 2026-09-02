package correlation_test

import (
	"testing"

	"github.com/trnahnh/idemio/internal/correlation"
)

const key = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// Every probe depends on this being reproducible from the key alone. If it ever stopped
// being deterministic, crash recovery would silently find nothing and escalate healthy
// writes to indeterminate.
func TestTheSameKeyAlwaysProducesTheSameID(t *testing.T) {
	first := correlation.ID("agent-checkout-flow", key)
	second := correlation.ID("agent-checkout-flow", key)

	if first != second {
		t.Fatalf("%s != %s: the correlation id is not deterministic", first, second)
	}
	if first == "" {
		t.Fatal("the correlation id is empty")
	}
}

func TestDifferentAgentsGetDifferentIDsForTheSameKey(t *testing.T) {
	if correlation.ID("agent-a", key) == correlation.ID("agent-b", key) {
		t.Fatal("two agents share a correlation id; ADR-0002 scopes keys per agent, so a probe " +
			"would attribute one agent's execution to another")
	}
}

// The separator matters: without it, ("ab", "c") and ("a", "bc") would collide.
func TestTheAgentAndKeyBoundaryIsUnambiguous(t *testing.T) {
	if correlation.ID("ab", "c") == correlation.ID("a", "bc") {
		t.Fatal("the agent and key are concatenated without an unambiguous boundary")
	}
}

func TestTheIDIsAStableLength(t *testing.T) {
	short := correlation.ID("a", "b")
	long := correlation.ID("agent-with-a-very-long-name-indeed", key)

	if len(short) != len(long) {
		t.Fatalf("ids vary in length: %d and %d", len(short), len(long))
	}
	for _, c := range short {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Fatalf("id %q is not hex; it travels in a header and a query string", short)
		}
	}
}
