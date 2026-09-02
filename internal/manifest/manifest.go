package manifest

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

type StatusRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

func (r StatusRange) contains(status int) bool {
	return status >= r.From && status <= r.To
}

type ErrorClassification struct {
	Definitive  []StatusRange `json:"definitive"`
	NotExecuted []StatusRange `json:"not_executed"`
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

const (
	ClassCreate  = "create"
	ClassReplace = "replace"
	ClassMutate  = "mutate"
	ClassAppend  = "append"
	ClassDelete  = "delete"
)

var classes = []string{ClassCreate, ClassReplace, ClassMutate, ClassAppend, ClassDelete}

type Operation struct {
	Class string   `json:"class"`
	Scope []string `json:"scope"`
}

type document struct {
	ResourceType     string               `json:"resource_type"`
	ConflictWindowMS int                  `json:"conflict_window_ms"`
	Enforce          bool                 `json:"enforce"`
	ProbePath        string               `json:"probe_path"`
	Operations       map[string]Operation `json:"operations"`
	Errors           ErrorClassification  `json:"errors"`
}

type Definition struct {
	Type           string
	ConflictWindow time.Duration
	Enforce        bool
	ProbePath      string
	Operations     map[string]Operation
	Errors         ErrorClassification
}

func (d document) definition() Definition {
	return Definition{
		Type:           d.ResourceType,
		ConflictWindow: time.Duration(d.ConflictWindowMS) * time.Millisecond,
		Enforce:        d.Enforce,
		ProbePath:      d.ProbePath,
		Operations:     d.Operations,
		Errors:         d.Errors,
	}
}

// A file is validated against the stem of its own name so that a copied manifest whose
// resource_type was not updated is a boot failure rather than a silently shadowed type.
func (d document) validate(stem string) []string {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", stem, fmt.Sprintf(format, args...)))
	}

	if d.ResourceType == "" {
		fail("resource_type is empty")
	} else if d.ResourceType != stem {
		fail("declares resource_type %q but is named %q; the file name is the type", d.ResourceType, stem)
	}

	if d.ConflictWindowMS <= 0 {
		fail("conflict_window_ms must be positive; a zero window disables conflict detection " +
			"for this type without saying so")
	}
	if !strings.HasPrefix(d.ProbePath, "/") {
		fail("probe_path %q is not a path; crash recovery without a probe must be an "+
			"explicit, recorded acceptance", d.ProbePath)
	}
	if len(d.Operations) == 0 {
		fail("declares no operations")
	}
	if !d.Errors.declared() {
		fail("declares no error classification; every downstream status would fall to indeterminate")
	}

	for _, r := range slices.Concat(d.Errors.Definitive, d.Errors.NotExecuted) {
		if r.From < 100 || r.To > 599 || r.From > r.To {
			fail("status range %d-%d is not a range of HTTP statuses", r.From, r.To)
		}
	}
	for _, definitive := range d.Errors.Definitive {
		for _, notExecuted := range d.Errors.NotExecuted {
			if definitive.From <= notExecuted.To && notExecuted.From <= definitive.To {
				fail("status ranges %d-%d and %d-%d overlap; a status cannot be both "+
					"definitive and provably not executed",
					definitive.From, definitive.To, notExecuted.From, notExecuted.To)
			}
		}
	}

	for name, operation := range d.Operations {
		if !slices.Contains(classes, operation.Class) {
			fail("operation %q has class %q, which is not one of %s",
				name, operation.Class, strings.Join(classes, ", "))
		}
		if len(operation.Scope) > 0 && operation.Class != ClassMutate {
			fail("operation %q declares a scope selector but has class %q; selectors are "+
				"only consulted between two mutates", name, operation.Class)
		}
		for _, path := range operation.Scope {
			if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") ||
				strings.Contains(path, "..") {
				fail("operation %q has scope path %q, which is not a field path", name, path)
			}
		}
	}

	return problems
}
