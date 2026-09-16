package main

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestShouldWarnImplicitBlocksDefault is the D1 guard test: a dep add edge
// created with the implicit type=blocks default must warn on stderr that the
// edge is type=blocks and that structural parent/child linkage requires -t
// parent-child, so children don't silently drop from bd ready. At the command
// layer, explicit is true when the user passed -t (any value, including
// blocks) or the --blocked-by/--depends-on aliases.
//
// The warning fires on the documented-default majority path, so it is scoped
// to an interactive operator: a non-TTY stderr (scripted and agent callers,
// CI, `bd dep add 2>log`), --quiet, and BD_NO_DEP_TYPE_WARNING each suppress
// it. Those three cases are the anti-inversion controls — without them the
// warning would train operators and agents to ignore stderr on the correct
// path.
func TestShouldWarnImplicitBlocksDefault(t *testing.T) {
	tests := []struct {
		name             string
		dt               types.DependencyType
		explicit         bool
		quiet            bool
		noWarnEnv        string
		stderrIsTerminal bool
		want             bool
	}{
		{name: "implicit blocks default on a TTY warns", dt: types.DepBlocks, stderrIsTerminal: true, want: true},
		{name: "explicit blocks (-t or --blocked-by/--depends-on) does not warn", dt: types.DepBlocks, explicit: true, stderrIsTerminal: true, want: false},
		{name: "parent-child default does not warn", dt: types.DepParentChild, stderrIsTerminal: true, want: false},
		{name: "tracks default does not warn", dt: types.DepTracks, stderrIsTerminal: true, want: false},
		{name: "non-TTY stderr does not warn (scripted and agent callers)", dt: types.DepBlocks, stderrIsTerminal: false, want: false},
		{name: "--quiet does not warn", dt: types.DepBlocks, quiet: true, stderrIsTerminal: true, want: false},
		{name: "BD_NO_DEP_TYPE_WARNING does not warn", dt: types.DepBlocks, noWarnEnv: "1", stderrIsTerminal: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldWarnImplicitBlocksDefault(tt.dt, tt.explicit, tt.quiet, tt.noWarnEnv, tt.stderrIsTerminal)
			if got != tt.want {
				t.Errorf("shouldWarnImplicitBlocksDefault(%v, explicit=%v, quiet=%v, env=%q, tty=%v) = %v, want %v",
					tt.dt, tt.explicit, tt.quiet, tt.noWarnEnv, tt.stderrIsTerminal, got, tt.want)
			}
		})
	}
}

// TestEmitImplicitBlocksDefaultWarning locks the message content. It calls the
// emitter directly because captureStderr replaces stderr with a pipe, which is
// exactly the non-TTY case the gate above suppresses.
func TestEmitImplicitBlocksDefaultWarning(t *testing.T) {
	got := captureStderr(t, emitImplicitBlocksDefaultWarning)

	if !strings.Contains(got, "type=blocks") {
		t.Errorf("warning must state the edge is type=blocks, got %q", got)
	}
	if !strings.Contains(got, "-t parent-child") {
		t.Errorf("warning must name -t parent-child for structural linkage, got %q", got)
	}
	if !strings.Contains(got, "bd ready") {
		t.Errorf("warning must explain the bd ready impact, got %q", got)
	}
	if !strings.Contains(got, "--quiet") || !strings.Contains(got, "BD_NO_DEP_TYPE_WARNING") {
		t.Errorf("warning must name both suppression knobs, got %q", got)
	}
}

// TestWarnImplicitBlocksDefaultStaysSilentUnderCapturedStderr is the
// end-to-end control: the command-layer entry point must print nothing when
// stderr is not a terminal, which is every scripted and agent invocation.
func TestWarnImplicitBlocksDefaultStaysSilentUnderCapturedStderr(t *testing.T) {
	got := captureStderr(t, func() {
		warnImplicitBlocksDefault(types.DepBlocks, false)
	})
	if got != "" {
		t.Errorf("expected no warning when stderr is not a terminal, got %q", got)
	}
}

// TestParentBlocksOwnChild is the #6506 guard test: `bd dep add P C` where C is
// already P's parent-child child is the "close gate on the epic" idiom, and it
// must be reported rather than refused — since the cascade fix the edge is
// harmless, and it is what an operator wiring a close gate actually meant.
//
// The predicate reads the parent's DEPENDENTS, so the two controls that matter
// are a dependent with the right id and the wrong edge type (a plain blocks
// edge onto the parent is an ordinary cycle, not a close gate) and a
// parent-child dependent that is not the blocker at all (P's other children
// must not make every blocker look like a close gate).
func TestParentBlocksOwnChild(t *testing.T) {
	dependent := func(id string, dt types.DependencyType) *types.IssueWithDependencyMetadata {
		return &types.IssueWithDependencyMetadata{Issue: types.Issue{ID: id}, DependencyType: dt}
	}

	tests := []struct {
		name       string
		dependents []*types.IssueWithDependencyMetadata
		childID    string
		want       bool
	}{
		{
			name:       "blocker is the parent's own child",
			dependents: []*types.IssueWithDependencyMetadata{dependent("bd-c1", types.DepParentChild)},
			childID:    "bd-c1",
			want:       true,
		},
		{
			name: "blocker is one of several children",
			dependents: []*types.IssueWithDependencyMetadata{
				dependent("bd-c1", types.DepParentChild),
				dependent("bd-c2", types.DepParentChild),
			},
			childID: "bd-c2",
			want:    true,
		},
		{
			name:       "dependent with the right id but a blocks edge is not a child",
			dependents: []*types.IssueWithDependencyMetadata{dependent("bd-c1", types.DepBlocks)},
			childID:    "bd-c1",
			want:       false,
		},
		{
			name:       "the parent's other children do not match",
			dependents: []*types.IssueWithDependencyMetadata{dependent("bd-c1", types.DepParentChild)},
			childID:    "bd-x",
			want:       false,
		},
		{name: "no dependents at all", childID: "bd-c1", want: false},
		{
			name:       "a nil row is skipped, not dereferenced",
			dependents: []*types.IssueWithDependencyMetadata{nil, dependent("bd-c1", types.DepParentChild)},
			childID:    "bd-c1",
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parentBlocksOwnChild(tt.dependents, tt.childID); got != tt.want {
				t.Errorf("parentBlocksOwnChild(%v, %q) = %v, want %v", tt.dependents, tt.childID, got, tt.want)
			}
		})
	}
}

// TestEmitParentBlocksOwnChildWarning locks the message content: it must name
// both beads, say the parent stays blocked, and say the child is NOT hidden
// from bd ready — the half an operator cannot verify without running a query,
// and the half that used to be false.
func TestEmitParentBlocksOwnChildWarning(t *testing.T) {
	got := captureStderr(t, func() { emitParentBlocksOwnChildWarning("bd-epic", "bd-child") })

	for _, want := range []string{"bd-epic", "bd-child", "bd ready", "6506"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning must mention %q, got %q", want, got)
		}
	}
	if !strings.Contains(got, "stays blocked") {
		t.Errorf("warning must say the parent stays blocked, got %q", got)
	}
	if !strings.Contains(got, "NOT hidden") {
		t.Errorf("warning must say the child is not hidden from ready work, got %q", got)
	}
}

// TestWarnIfParentBlocksOwnChildIgnoresNonBlockingTypes is the type gate: the
// advisory is about a BLOCKING edge onto a child. A parent-child edge (or any
// other type) onto one's own child is not a close gate and must stay silent —
// and the gate must return before touching the store, which a nil store proves.
func TestWarnIfParentBlocksOwnChildIgnoresNonBlockingTypes(t *testing.T) {
	for _, dt := range []types.DependencyType{types.DepParentChild, types.DepWaitsFor, types.DepTracks} {
		got := captureStderr(t, func() {
			warnIfParentBlocksOwnChild(context.Background(), nil, "bd-epic", "bd-child", dt)
		})
		if got != "" {
			t.Errorf("type %s: expected silence, got %q", dt, got)
		}
	}
}
