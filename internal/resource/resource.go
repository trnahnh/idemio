package resource

import (
	"fmt"
	"slices"
	"strings"
)

type StatusRange struct {
	From int
	To   int
}

func (r StatusRange) contains(status int) bool {
	return status >= r.From && status <= r.To
}

type ErrorClassification struct {
	Definitive  []StatusRange
	NotExecuted []StatusRange
}

func (c ErrorClassification) declared() bool {
	return len(c.Definitive) > 0 || len(c.NotExecuted) > 0
}

func (c ErrorClassification) IsDefinitive(status int) bool {
	return slices.ContainsFunc(c.Definitive, func(r StatusRange) bool { return r.contains(status) })
}

func (c ErrorClassification) IsNotExecuted(status int) bool {
	return slices.ContainsFunc(c.NotExecuted, func(r StatusRange) bool { return r.contains(status) })
}

type Definition struct {
	Type       string
	Operations map[string]string
	ProbePath  string
	Errors     ErrorClassification
}

var registry = map[string]Definition{
	"invoice": {
		Type: "invoice",
		Operations: map[string]string{
			"create_charge": "create",
		},
		ProbePath: "/probe",
		Errors: ErrorClassification{
			Definitive: []StatusRange{{From: 200, To: 299}, {From: 400, To: 499}},
		},
	},
}

func Lookup(resourceType string) (Definition, bool) {
	definition, ok := registry[resourceType]
	return definition, ok
}

func ClassOf(resourceType, operation string) (string, bool) {
	definition, ok := registry[resourceType]
	if !ok {
		return "", false
	}
	class, ok := definition.Operations[operation]
	return class, ok
}

func ClassificationFor(resourceType string) ErrorClassification {
	return registry[resourceType].Errors
}

func Validate() error {
	var problems []string

	for name, definition := range registry {
		if len(definition.Operations) == 0 {
			problems = append(problems, fmt.Sprintf("%s declares no operations", name))
		}
		if definition.ProbePath == "" {
			problems = append(problems, fmt.Sprintf(
				"%s declares no probe path; crash recovery for it would be manual and that "+
					"must be an explicit, recorded acceptance", name))
		}
		if !definition.Errors.declared() {
			problems = append(problems, fmt.Sprintf(
				"%s declares no error classification; every downstream status would fall to "+
					"indeterminate", name))
		}
	}

	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("resource registry: %s", strings.Join(problems, "; "))
	}
	return nil
}
