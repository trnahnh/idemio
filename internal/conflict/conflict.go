package conflict

import (
	"fmt"
	"strings"

	"github.com/trnahnh/idemio/internal/manifest"
)

// ADR-0007's matrix. Every pair conflicts except two appends, an append beside a mutate,
// and two mutates whose scope selectors are disjoint.
func Compatible(a, b manifest.Operation) bool {
	switch {
	case a.Class == manifest.ClassAppend && b.Class == manifest.ClassAppend:
		return true
	case a.Class == manifest.ClassAppend && b.Class == manifest.ClassMutate,
		a.Class == manifest.ClassMutate && b.Class == manifest.ClassAppend:
		return true
	case a.Class == manifest.ClassMutate && b.Class == manifest.ClassMutate:
		return ScopesDisjoint(a.Scope, b.Scope)
	default:
		return false
	}
}

// An operation that declares no selector writes the whole resource, so it overlaps every
// other selector including the empty one.
func ScopesDisjoint(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, left := range a {
		for _, right := range b {
			if overlaps(left, right) {
				return false
			}
		}
	}
	return true
}

// Prefix containment, not string equality: an operation writing "billing_address" does
// write "billing_address.city", and calling those disjoint would miss a real conflict.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

func Reason(incoming, existing manifest.Operation) string {
	if incoming.Class == manifest.ClassMutate && existing.Class == manifest.ClassMutate {
		return fmt.Sprintf("mutate/mutate with overlapping scope %s and %s",
			scopeText(incoming.Scope), scopeText(existing.Scope))
	}
	return fmt.Sprintf("%s/%s", incoming.Class, existing.Class)
}

func scopeText(scope []string) string {
	if len(scope) == 0 {
		return "(whole resource)"
	}
	return "[" + strings.Join(scope, " ") + "]"
}
