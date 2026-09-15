package contracts

import (
	"fmt"
	"regexp"
	"slices"
)

// SandboxListFilter intersects Metadata containment with optional state and ID sets.
type SandboxListFilter struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	States   []string          `json:"states,omitempty"`
	IDs      []string          `json:"ids,omitempty"`
}

var sandboxFilterIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// Normalize validates bounded sets and canonicalizes their cursor identity.
func (filter SandboxListFilter) Normalize() (SandboxListFilter, error) {
	if len(filter.States) > 9 || len(filter.IDs) > 64 {
		return SandboxListFilter{}, fmt.Errorf("SecondBox Sandbox filter exceeds 9 states or 64 IDs")
	}
	for _, state := range filter.States {
		switch state {
		case SandboxStateCreating, SandboxStateStopped, SandboxStateStarting, SandboxStateReady, SandboxStateDraining, SandboxStateStopping, SandboxStateFailed, SandboxStateDeleting, SandboxStateDeleted:
		default:
			return SandboxListFilter{}, fmt.Errorf("SecondBox Sandbox state filter is invalid")
		}
	}
	for _, id := range filter.IDs {
		if !sandboxFilterIDPattern.MatchString(id) {
			return SandboxListFilter{}, fmt.Errorf("SecondBox Sandbox ID filter is invalid")
		}
	}
	filter.States = slices.Clone(filter.States)
	filter.IDs = slices.Clone(filter.IDs)
	slices.Sort(filter.States)
	slices.Sort(filter.IDs)
	filter.States = slices.Compact(filter.States)
	filter.IDs = slices.Compact(filter.IDs)
	return filter, nil
}
