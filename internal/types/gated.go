package types

import "strings"

// This file holds the ONE rule by which an issue counts as GATED, and the
// projection of a gate onto the wire.
//
// GATED IS DERIVED, NEVER STORED. `bd gate create --blocks X` writes a gate
// issue plus an ordinary blocking dependency X -> gate; nothing on X changes.
// `bd ready` then drops X through the denormalized is_blocked column, whose
// recompute (internal/storage/issueops/blocked_state.go,
// shouldBeBlockedIDsUnionScopedSQL) marks an issue blocked when it has a
// blocking-edge dependency — blocks / conditional-blocks / waits-for — on a
// target that is neither closed nor pinned. IsActiveGate is that same rule
// restricted to gate-typed targets, so a surface that renders GATED cannot
// disagree with the readiness query about whether the gate is still holding.

// GateRef is a gate projected for a caller: the id to resolve, what kind of
// wait it is, and why. It is what `gated_by` carries on the detail view.
type GateRef struct {
	ID string `json:"id"`
	// Type is the gate's await condition (human, timer, gh:run, gh:pr, bead);
	// "gate" when the issue carries no await type, which is what a gate made
	// with a bare `bd create --type gate` looks like.
	Type string `json:"type,omitempty"`
	// Reason is the free text `bd gate create --reason` recorded, empty when
	// the gate was created without one.
	Reason string `json:"reason,omitempty"`
}

// IsActiveGate reports whether target, reached over a dependency of type
// depType, is a gate that is currently holding its dependent back.
//
// THE one predicate: every gated decoration — `bd show`'s header and meta
// lines, `bd list`'s glyph, the detail view's gated_by — asks this and nothing
// else. The three clauses mirror the is_blocked recompute leg by leg: a
// blocking edge (IsBlockingEdge, which is 'blocks' OR 'conditional-blocks' OR
// 'waits-for'), a target that is not closed and not pinned, and — the only
// restriction this adds — a target that is a gate rather than ordinary work.
func IsActiveGate(depType DependencyType, target *Issue) bool {
	if target == nil || target.IssueType != TypeGate {
		return false
	}
	if !depType.IsBlockingEdge() {
		return false
	}
	return target.Status != StatusClosed && target.Status != StatusPinned
}

// ActiveGates selects, from an issue's hydrated dependencies, the gates that
// are actively blocking it, in the order the dependencies arrived.
func ActiveGates(deps []*IssueWithDependencyMetadata) []*Issue {
	var gates []*Issue
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		issue := dep.Issue
		if IsActiveGate(dep.DependencyType, &issue) {
			gate := issue
			gates = append(gates, &gate)
		}
	}
	return gates
}

// GateKind names a gate's await condition for display, falling back to the
// issue type when the gate carries none.
func GateKind(gate *Issue) string {
	if gate == nil {
		return ""
	}
	if gate.AwaitType != "" {
		return gate.AwaitType
	}
	return string(TypeGate)
}

// NewGateRef projects one gate issue onto the wire shape.
func NewGateRef(gate *Issue) GateRef {
	if gate == nil {
		return GateRef{}
	}
	return GateRef{
		ID:     gate.ID,
		Type:   GateKind(gate),
		Reason: GateReason(gate.Description),
	}
}

// GateRefs projects a gate set, returning nil for an empty one so the field
// stays absent rather than serializing an empty array.
func GateRefs(gates []*Issue) []GateRef {
	if len(gates) == 0 {
		return nil
	}
	refs := make([]GateRef, 0, len(gates))
	for _, gate := range gates {
		refs = append(refs, NewGateRef(gate))
	}
	return refs
}

// gateReasonMarker separates an ad-hoc gate's boilerplate description from the
// reason its creator gave. The reason has no column of its own, so the
// description is where it lives; GateDescription writes it and GateReason
// reads it back, and they sit together so the pair cannot drift.
const gateReasonMarker = "\n\nReason: "

// GateDescription builds an ad-hoc gate's description. `bd gate create` is its
// only writer.
func GateDescription(targetID, reason string) string {
	desc := "Ad-hoc gate blocking " + targetID
	if reason != "" {
		desc += gateReasonMarker + reason
	}
	return desc
}

// GateReason recovers the reason GateDescription recorded, or "" when the gate
// carries none (including every gate not created by `bd gate create`).
func GateReason(description string) string {
	idx := strings.Index(description, gateReasonMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(description[idx+len(gateReasonMarker):])
}
