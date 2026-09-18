package main

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// gatedIssueIDs returns the subset of a listing's ids that an OPEN gate is
// holding back, so the row can carry ui.StatusIconGated instead of a plain ○.
//
// BATCHED BY ID SET, never per row: the dependency rows for the whole page
// come back in one read (or from the map the tree view has already loaded),
// and the gate candidates they name are hydrated in a second. A listing
// therefore costs at most two extra queries whatever its length.
//
// The verdict itself is types.IsActiveGate — the one predicate the readiness
// query's is_blocked leg encodes and `bd show` renders from — so a glyph here
// and a GATED header there cannot say different things about the same gate.
//
// Best effort: a read that fails yields no decoration rather than failing the
// listing, matching how `bd list` already treats its blocking annotation.
func gatedIssueIDs(
	ctx context.Context,
	st storage.DoltStorage,
	issues []*types.Issue,
	preloadedDeps map[string][]*types.Dependency,
) map[string]bool {
	if st == nil || len(issues) == 0 {
		return nil
	}

	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue == nil || issue.Status == types.StatusClosed {
			continue
		}
		ids = append(ids, issue.ID)
	}
	if len(ids) == 0 {
		return nil
	}

	depsByIssue := preloadedDeps
	if depsByIssue == nil {
		var err error
		depsByIssue, err = st.GetDependencyRecordsForIssues(ctx, ids)
		if err != nil {
			return nil
		}
	}

	// Candidate targets: the blocking edges of the ids on this page. A gate
	// reached over a non-blocking edge (relates-to, discovered-from) does not
	// hold anything back, and the predicate would reject it anyway; skipping
	// it here just keeps the hydration batch small.
	var targetIDs []string
	seen := make(map[string]bool)
	for _, id := range ids {
		for _, dep := range depsByIssue[id] {
			if dep == nil || !dep.Type.IsBlockingEdge() || seen[dep.DependsOnID] {
				continue
			}
			seen[dep.DependsOnID] = true
			targetIDs = append(targetIDs, dep.DependsOnID)
		}
	}
	if len(targetIDs) == 0 {
		return nil
	}

	targets, err := st.GetIssuesByIDs(ctx, targetIDs)
	if err != nil {
		return nil
	}
	targetByID := make(map[string]*types.Issue, len(targets))
	for _, target := range targets {
		if target != nil {
			targetByID[target.ID] = target
		}
	}

	gated := make(map[string]bool)
	for _, id := range ids {
		for _, dep := range depsByIssue[id] {
			if dep == nil {
				continue
			}
			if types.IsActiveGate(dep.Type, targetByID[dep.DependsOnID]) {
				gated[id] = true
				break
			}
		}
	}
	if len(gated) == 0 {
		return nil
	}
	return gated
}
