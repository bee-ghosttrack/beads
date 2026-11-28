package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// newReclaimTestCmd builds a command with the real reclaim scope flag set so the
// parser under test is exactly the one production uses (registerReclaimScopeFlags
// is shared with init()).
func newReclaimTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "reclaim", RunE: func(*cobra.Command, []string) error { return nil }}
	registerReclaimScopeFlags(cmd.Flags())
	return cmd
}

func TestReclaimFilterFromFlagsUnset(t *testing.T) {
	cmd := newReclaimTestCmd()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	filter, err := reclaimFilterFromFlags(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filter.IsEmpty() {
		t.Fatalf("no flags => filter %+v, want empty (global sweep)", filter)
	}
}

func TestReclaimFilterFromFlagsPopulated(t *testing.T) {
	cmd := newReclaimTestCmd()
	args := []string{
		"--id", "wy-a", "--id", "wy-b",
		"--assignee", "zelda",
		"--label", "lane-a",
		"--label-any", "x,y",
		"--exclude-label", "pinned",
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	filter, err := reclaimFilterFromFlags(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filter.IsEmpty() {
		t.Fatal("populated flags => empty filter")
	}
	if len(filter.IDs) != 2 || filter.IDs[0] != "wy-a" || filter.IDs[1] != "wy-b" {
		t.Errorf("IDs = %v", filter.IDs)
	}
	if len(filter.Assignees) != 1 || filter.Assignees[0] != "zelda" {
		t.Errorf("Assignees = %v", filter.Assignees)
	}
	if len(filter.Labels) != 1 || filter.Labels[0] != "lane-a" {
		t.Errorf("Labels = %v", filter.Labels)
	}
	if len(filter.LabelsAny) != 2 {
		t.Errorf("LabelsAny = %v", filter.LabelsAny)
	}
	if len(filter.ExcludeLabels) != 1 || filter.ExcludeLabels[0] != "pinned" {
		t.Errorf("ExcludeLabels = %v", filter.ExcludeLabels)
	}
}

// A scope flag that parses to no usable value is operator error: it must be a
// hard error, never a silent degrade into a global sweep.
func TestReclaimFilterFromFlagsRejectsEmptyValue(t *testing.T) {
	for _, flag := range []string{"--label", "--label-any", "--assignee", "--id", "--exclude-label"} {
		t.Run(flag, func(t *testing.T) {
			cmd := newReclaimTestCmd()
			if err := cmd.ParseFlags([]string{flag, ""}); err != nil {
				t.Fatal(err)
			}
			if _, err := reclaimFilterFromFlags(cmd); err == nil {
				t.Fatalf("%s '' => nil error, want a hard error", flag)
			}
		})
	}
}

// TestReclaimFilterAnyReplica pins the override flag's parse: off by default
// (the granting-replica guard is armed unless an operator asks for it), on when
// given, and never mistaken for a scope. The empty-value hard error that guards
// the string-slice flags does not apply to a bool — `--any-replica` with no
// value is the flag, not a degenerate scope.
func TestReclaimFilterAnyReplica(t *testing.T) {
	cmd := newReclaimTestCmd()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	filter, err := reclaimFilterFromFlags(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filter.AnyReplica {
		t.Error("AnyReplica defaulted to true; the replica guard must be armed by default")
	}

	cmd = newReclaimTestCmd()
	if err := cmd.ParseFlags([]string{"--any-replica", "--label", "lane-a"}); err != nil {
		t.Fatal(err)
	}
	filter, err = reclaimFilterFromFlags(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filter.AnyReplica {
		t.Error("--any-replica did not set AnyReplica")
	}
	if filter.IsEmpty() {
		t.Error("--label lane-a alongside --any-replica reported an empty (global) filter")
	}
}

// TestReclaimCommandDeclaresDryRun pins the flag on the real command, off by
// default so a bare `bd reclaim` still sweeps, and its place in the help.
func TestReclaimCommandDeclaresDryRun(t *testing.T) {
	flag := reclaimCmd.Flags().Lookup("dry-run")
	if flag == nil {
		t.Fatal("bd reclaim has no --dry-run flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("--dry-run defaults to %q; a bare reclaim must still sweep", flag.DefValue)
	}
	if !strings.Contains(reclaimCmd.Long, "bd reclaim --dry-run") {
		t.Error("the Long help carries no `bd reclaim --dry-run` example")
	}
}

// TestRenderReclaimDryRun pins what a preview prints. The rows read the same
// as a sweep's; only the headline's verb changes, and the zero-row lines are
// the sweep's own.
func TestRenderReclaimDryRun(t *testing.T) {
	rows := []types.ReclaimedLease{
		{ID: "bd-1", PreviousOwner: "worker-a"},
		{ID: "bd-2"},
	}
	for _, tc := range []struct {
		name    string
		rows    []types.ReclaimedLease
		scoped  bool
		dryRun  bool
		want    []string
		wantNot []string
	}{
		{
			name: "a preview with rows", rows: rows, dryRun: true,
			want:    []string{"Would reclaim 2 stale-lease issue(s):", "  bd-1 (was held by worker-a)", "  bd-2 (was held by (unassigned))"},
			wantNot: []string{"Reclaimed 2"},
		},
		{
			name: "a sweep with rows", rows: rows,
			want:    []string{"Reclaimed 2 stale-lease issue(s):", "  bd-1 (was held by worker-a)"},
			wantNot: []string{"Would reclaim"},
		},
		{name: "a preview with none", dryRun: true, want: []string{"No stale leases to reclaim"}, wantNot: []string{"Would reclaim"}},
		{name: "a scoped preview with none", dryRun: true, scoped: true, want: []string{"No stale leases to reclaim in the filtered scope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := jsonOutput
			jsonOutput = false
			t.Cleanup(func() { jsonOutput = prev })
			out := captureStdout(t, func() error { return renderReclaim(tc.rows, tc.scoped, tc.dryRun) })
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output %q does not contain %q", out, want)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(out, unwanted) {
					t.Errorf("output %q contains %q", out, unwanted)
				}
			}
		})
	}
}

// TestRenderReclaimJSONCarriesDryRun pins the machine-readable key in every
// case, a sweep and the empty answer included, so a consumer can always tell a
// preview's list from a sweep's without knowing which flags it passed.
func TestRenderReclaimJSONCarriesDryRun(t *testing.T) {
	t.Setenv("BD_JSON_ENVELOPE", "")
	for _, tc := range []struct {
		name   string
		rows   []types.ReclaimedLease
		dryRun bool
	}{
		{"a preview with rows", []types.ReclaimedLease{{ID: "bd-1", PreviousOwner: "w"}}, true},
		{"a preview with none", []types.ReclaimedLease{}, true},
		{"a sweep with rows", []types.ReclaimedLease{{ID: "bd-1", PreviousOwner: "w", Revision: "7"}}, false},
		{"a sweep with none", []types.ReclaimedLease{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := jsonOutput
			jsonOutput = true
			t.Cleanup(func() { jsonOutput = prev })
			out := captureStdout(t, func() error { return renderReclaim(tc.rows, false, tc.dryRun) })
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			value, present := got["dry_run"]
			if !present {
				t.Fatalf("JSON %s has no dry_run key", out)
			}
			if value != tc.dryRun {
				t.Errorf("dry_run = %v, want %v", value, tc.dryRun)
			}
			if count, _ := got["count"].(float64); int(count) != len(tc.rows) {
				t.Errorf("count = %v, want %d", got["count"], len(tc.rows))
			}
		})
	}
}

// scriptedReclaimer answers every request with one fixed result and keeps the
// request, so a test can stand in for a role that does or does not know DryRun.
type scriptedReclaimer struct {
	result issueops.ReclaimResult
	got    issueops.ReclaimRequest
}

func (r *scriptedReclaimer) Reclaim(_ context.Context, request issueops.ReclaimRequest) (issueops.ReclaimResult, error) {
	r.got = request
	return r.result, nil
}

// TestRunReclaimRefusesAPreviewTheRoleDidNotConfirm pins the command against a
// LeaseReclaimer that predates ReclaimRequest.DryRun. Such a role ignores the
// member, reverts the rows and answers DryRun false. The command must not
// print those rows as a preview: it fails, and it counts as a write when rows
// were named, because they may have been reverted.
func TestRunReclaimRefusesAPreviewTheRoleDidNotConfirm(t *testing.T) {
	reverted := []types.ReclaimedLease{
		{ID: "bd-1", PreviousOwner: "alice", Revision: "rev-1"},
		{ID: "bd-2", Revision: "rev-2"},
	}
	previewed := []types.ReclaimedLease{{ID: "bd-1", PreviousOwner: "alice"}}
	cases := []struct {
		name      string
		dryRun    bool
		json      bool
		result    issueops.ReclaimResult
		wantErr   bool
		wantWrite bool
		want      []string
		wantNot   []string
	}{
		{
			name: "a preview the role ran as a sweep", dryRun: true,
			result:  issueops.ReclaimResult{Reclaimed: reverted},
			wantErr: true, wantWrite: true,
			wantNot: []string{"Would reclaim", "Reclaimed", "bd-1", "bd-2"},
		},
		{
			name: "the same under --json", dryRun: true, json: true,
			result:  issueops.ReclaimResult{Reclaimed: reverted},
			wantErr: true, wantWrite: true,
			want:    []string{`"error"`},
			wantNot: []string{`"dry_run"`, `"reclaimed"`},
		},
		{
			name: "an unconfirmed preview that named nothing", dryRun: true,
			result:  issueops.ReclaimResult{Reclaimed: []types.ReclaimedLease{}},
			wantErr: true,
			wantNot: []string{"No stale leases"},
		},
		{
			name: "a confirmed preview", dryRun: true,
			result: issueops.ReclaimResult{DryRun: true, Reclaimed: previewed},
			want:   []string{"Would reclaim 1 stale-lease issue(s):", "bd-1 (was held by alice)"},
		},
		{
			name:      "a sweep",
			result:    issueops.ReclaimResult{Reclaimed: reverted},
			wantWrite: true,
			want:      []string{"Reclaimed 2 stale-lease issue(s):", "bd-2 (was held by (unassigned))"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldJSON := jsonOutput
			jsonOutput = tc.json
			commandDidWrite.Store(false)
			t.Cleanup(func() {
				jsonOutput = oldJSON
				commandDidWrite.Store(false)
			})

			reclaimer := &scriptedReclaimer{result: tc.result}
			out, err := captureStdoutWithError(t, func() error {
				return runReclaim(context.Background(), reclaimer, issueops.ReclaimRequest{Actor: "tester", DryRun: tc.dryRun}, false)
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runReclaim error = %v, want an error: %v", err, tc.wantErr)
			}
			if reclaimer.got.DryRun != tc.dryRun {
				t.Errorf("the role was asked DryRun = %v, want %v", reclaimer.got.DryRun, tc.dryRun)
			}
			if got := commandDidWrite.Load(); got != tc.wantWrite {
				t.Errorf("commandDidWrite = %v, want %v", got, tc.wantWrite)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("stdout is missing %q:\n%s", want, out)
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(out, not) {
					t.Errorf("stdout contains %q:\n%s", not, out)
				}
			}
		})
	}
}

// TestReclaimUnconfirmedPreviewNamesTheRows pins what the refusal tells the operator:
// which rows the role named, and how many of them carry a revision, which a
// preview never mints and a revert always does.
func TestReclaimUnconfirmedPreviewNamesTheRows(t *testing.T) {
	preview := issueops.ReclaimRequest{Actor: "tester", DryRun: true}
	rows := []types.ReclaimedLease{
		{ID: "bd-1", PreviousOwner: "alice", Revision: "rev-1"},
		{ID: "bd-2"},
		{ID: "bd-3", PreviousOwner: "bob", Revision: "rev-3"},
	}

	err := unconfirmedPreview(preview, issueops.ReclaimResult{Reclaimed: rows})
	if err == nil {
		t.Fatal("a preview answered with DryRun false was accepted")
	}
	for _, want := range []string{
		"--dry-run was not confirmed",
		"It named 3 lease(s), of which 2 carry a new revision",
		"bd-1 (was held by alice)",
		"bd-2 (was held by (unassigned))",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q:\n%s", want, err)
		}
	}

	err = unconfirmedPreview(preview, issueops.ReclaimResult{Reclaimed: []types.ReclaimedLease{}})
	if err == nil || !strings.Contains(err.Error(), "It named no lease, so nothing was reverted this time") {
		t.Errorf("an unconfirmed preview with no rows answered %v", err)
	}

	for name, pair := range map[string]struct {
		request issueops.ReclaimRequest
		result  issueops.ReclaimResult
	}{
		"a confirmed preview": {preview, issueops.ReclaimResult{DryRun: true, Reclaimed: rows}},
		"a sweep":             {issueops.ReclaimRequest{Actor: "tester"}, issueops.ReclaimResult{Reclaimed: rows}},
	} {
		if err := unconfirmedPreview(pair.request, pair.result); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}
