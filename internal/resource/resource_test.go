package resource_test

import (
	"testing"

	"github.com/trnahnh/idemio/internal/resource"
)

func TestShippedRegistryPassesBootValidation(t *testing.T) {
	if err := resource.Validate(); err != nil {
		t.Fatalf("registry rejected at boot: %v", err)
	}
}

func TestEveryRegisteredTypeDeclaresAProbeAndAClassification(t *testing.T) {
	definition, ok := resource.Lookup("invoice")
	if !ok {
		t.Fatal("invoice is not registered")
	}
	if definition.ProbePath == "" {
		t.Error("invoice declares no probe path")
	}
	if !definition.Errors.IsDefinitive(200) || !definition.Errors.IsDefinitive(422) {
		t.Error("invoice does not classify success or business failure as definitive")
	}
}

// An unregistered type has no declared classification, so every status must fall through to
// indeterminate rather than being guessed at.
func TestUnregisteredTypeClassifiesNothing(t *testing.T) {
	classification := resource.ClassificationFor("never_onboarded")

	for _, status := range []int{200, 201, 400, 422, 500, 503} {
		if classification.IsDefinitive(status) {
			t.Errorf("status %d was treated as definitive for an unregistered type", status)
		}
		if classification.IsNotExecuted(status) {
			t.Errorf("status %d was treated as not-executed for an unregistered type", status)
		}
	}
}

func TestClassificationRangesAreInclusive(t *testing.T) {
	classification := resource.ErrorClassification{
		Definitive:  []resource.StatusRange{{From: 200, To: 299}},
		NotExecuted: []resource.StatusRange{{From: 503, To: 503}},
	}

	for status, want := range map[int]bool{199: false, 200: true, 299: true, 300: false} {
		if got := classification.IsDefinitive(status); got != want {
			t.Errorf("IsDefinitive(%d) = %v, want %v", status, got, want)
		}
	}
	if !classification.IsNotExecuted(503) || classification.IsNotExecuted(502) {
		t.Error("not-executed range is wrong")
	}
}
