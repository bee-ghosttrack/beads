package types

import "testing"

func gateIssue(id string, status Status) *Issue {
	return &Issue{ID: id, IssueType: TypeGate, Status: status, AwaitType: "human"}
}

func TestIsActiveGate(t *testing.T) {
	tests := []struct {
		name    string
		depType DependencyType
		target  *Issue
		want    bool
	}{
		{"open gate over a blocks edge", DepBlocks, gateIssue("g1", StatusOpen), true},
		{"open gate over conditional-blocks", DepConditionalBlocks, gateIssue("g1", StatusOpen), true},
		{"open gate over waits-for", DepWaitsFor, gateIssue("g1", StatusOpen), true},
		{"closed gate", DepBlocks, gateIssue("g1", StatusClosed), false},
		{"pinned gate", DepBlocks, gateIssue("g1", StatusPinned), false},
		// parent-child is structural: it propagates blockedness through the
		// is_blocked recompute, not as a direct blocking edge. Inheritance is
		// its own question (wy-raudbw) and must not sneak in here.
		{"gate over a parent-child edge", DepParentChild, gateIssue("g1", StatusOpen), false},
		{"gate over a relates-to edge", DepRelatesTo, gateIssue("g1", StatusOpen), false},
		{"open non-gate blocker", DepBlocks, &Issue{ID: "b1", IssueType: TypeTask, Status: StatusOpen}, false},
		{"nil target", DepBlocks, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsActiveGate(tt.depType, tt.target); got != tt.want {
				t.Errorf("IsActiveGate(%s, %v) = %v, want %v", tt.depType, tt.target, got, tt.want)
			}
		})
	}
}

func TestActiveGatesSelectsOnlyLiveGates(t *testing.T) {
	deps := []*IssueWithDependencyMetadata{
		{Issue: *gateIssue("open-gate", StatusOpen), DependencyType: DepBlocks},
		{Issue: *gateIssue("closed-gate", StatusClosed), DependencyType: DepBlocks},
		{Issue: Issue{ID: "plain", IssueType: TypeTask, Status: StatusOpen}, DependencyType: DepBlocks},
		{Issue: *gateIssue("second-gate", StatusOpen), DependencyType: DepConditionalBlocks},
		nil,
	}
	gates := ActiveGates(deps)
	if len(gates) != 2 {
		t.Fatalf("ActiveGates returned %d gates, want 2: %+v", len(gates), gates)
	}
	if gates[0].ID != "open-gate" || gates[1].ID != "second-gate" {
		t.Errorf("ActiveGates = %s,%s; want open-gate,second-gate in dependency order", gates[0].ID, gates[1].ID)
	}
}

func TestActiveGatesEmpty(t *testing.T) {
	if got := ActiveGates(nil); got != nil {
		t.Errorf("ActiveGates(nil) = %v, want nil", got)
	}
}

func TestGateDescriptionRoundTrip(t *testing.T) {
	desc := GateDescription("bd-abc", "Need design review")
	if got := GateReason(desc); got != "Need design review" {
		t.Errorf("GateReason(%q) = %q, want the reason back", desc, got)
	}
	bare := GateDescription("bd-abc", "")
	if got := GateReason(bare); got != "" {
		t.Errorf("GateReason(%q) = %q, want empty", bare, got)
	}
	if got := GateReason("a description written by something else"); got != "" {
		t.Errorf("GateReason on a foreign description = %q, want empty", got)
	}
}

func TestGateRefsProjection(t *testing.T) {
	if got := GateRefs(nil); got != nil {
		t.Errorf("GateRefs(nil) = %v, want nil so the field stays absent", got)
	}
	gate := &Issue{
		ID:          "g1",
		IssueType:   TypeGate,
		Status:      StatusOpen,
		AwaitType:   "gh:pr",
		Description: GateDescription("bd-abc", "waiting on review"),
	}
	refs := GateRefs([]*Issue{gate, {ID: "g2", IssueType: TypeGate, Status: StatusOpen}})
	if len(refs) != 2 {
		t.Fatalf("GateRefs returned %d refs, want 2", len(refs))
	}
	if refs[0] != (GateRef{ID: "g1", Type: "gh:pr", Reason: "waiting on review"}) {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	// A gate with no await type still names a kind rather than an empty one.
	if refs[1] != (GateRef{ID: "g2", Type: "gate"}) {
		t.Errorf("refs[1] = %+v, want the gate fallback kind and no reason", refs[1])
	}
}
