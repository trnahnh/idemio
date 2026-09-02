package conflict_test

import (
	"testing"

	"github.com/trnahnh/idemio/internal/conflict"
	"github.com/trnahnh/idemio/internal/manifest"
)

func op(class string, scope ...string) manifest.Operation {
	return manifest.Operation{Class: class, Scope: scope}
}

func TestMatrixMatchesADR0007(t *testing.T) {
	classes := []string{
		manifest.ClassCreate, manifest.ClassReplace, manifest.ClassMutate,
		manifest.ClassAppend, manifest.ClassDelete,
	}

	compatible := map[string]map[string]bool{
		manifest.ClassMutate: {manifest.ClassAppend: true},
		manifest.ClassAppend: {manifest.ClassAppend: true, manifest.ClassMutate: true},
	}

	// Both sides declare the same scope, so the mutate/mutate cell reads as the plain
	// conflict it is; the disjoint case is the exception, tested on its own below.
	for _, a := range classes {
		for _, b := range classes {
			want := compatible[a][b]
			if got := conflict.Compatible(op(a, "status"), op(b, "status")); got != want {
				t.Errorf("Compatible(%s, %s) = %v, want %v", a, b, got, want)
			}
		}
	}
}

func TestTwoMutatesConflictUnlessTheirScopesAreDisjoint(t *testing.T) {
	if !conflict.Compatible(op("mutate", "status"), op("mutate", "billing_address")) {
		t.Error("disjoint mutates conflict; the matrix is acting as a per-resource mutex")
	}
	if conflict.Compatible(op("mutate", "status"), op("mutate", "status")) {
		t.Error("two mutates on the same field are compatible")
	}
}

// The failure this guards is the one direction that matters: calling two scopes disjoint
// when they are not means a conflict is missed rather than one being invented.
func TestACoarseScopeContainsTheFinerOnesBelowIt(t *testing.T) {
	cases := []struct {
		a, b     []string
		disjoint bool
	}{
		{[]string{"billing_address"}, []string{"billing_address.city"}, false},
		{[]string{"billing_address.city"}, []string{"billing_address"}, false},
		{[]string{"billing_address.city"}, []string{"billing_address.country"}, true},
		{[]string{"status"}, []string{"status_history"}, true},
		{[]string{"a", "b"}, []string{"c", "b"}, false},
		{nil, []string{"status"}, false},
		{[]string{"status"}, nil, false},
		{nil, nil, false},
	}

	for _, tc := range cases {
		if got := conflict.ScopesDisjoint(tc.a, tc.b); got != tc.disjoint {
			t.Errorf("ScopesDisjoint(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.disjoint)
		}
	}
}

func TestAnOperationWithNoSelectorWritesTheWholeResource(t *testing.T) {
	if conflict.Compatible(op("mutate"), op("mutate", "status")) {
		t.Error("an undeclared scope was treated as narrow; it must mean the whole resource")
	}
}
