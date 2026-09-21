package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/oplog"
)

// setupOplogTest points oplog.dir at a temp dir with a known issue prefix and
// restores the package state afterwards.
func setupOplogTest(t *testing.T) string {
	t.Helper()
	if err := config.Initialize(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config.Set("oplog.dir", dir)
	config.Set("issue-prefix", "bd")
	t.Cleanup(func() {
		config.Set("oplog.dir", "")
		config.Set("issue-prefix", "")
		commandOp, oplogCmd, oplogArgs = nil, nil, nil
	})
	return dir
}

func readOplog(t *testing.T, dir string) []oplog.Record {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(matches) == 0 {
		return nil
	}
	if len(matches) != 1 {
		t.Fatalf("want one log file, got %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var recs []oplog.Record
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r oplog.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad record %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

func TestCommandOplog_CheckReadonlyBeginsOnce(t *testing.T) {
	dir := setupOplogTest(t)

	root := &cobra.Command{Use: "bd"}
	dep := &cobra.Command{Use: "dep"}
	add := &cobra.Command{Use: "add", Run: func(*cobra.Command, []string) {}}
	add.Flags().String("type", "blocks", "")
	add.Flags().String("note", "", "")
	root.AddCommand(dep)
	dep.AddCommand(add)
	if err := add.ParseFlags([]string{"--note", "private words"}); err != nil {
		t.Fatal(err)
	}

	stashCommandOplog(add, []string{"bd-a1", "bd-b2.3", "not an id"})
	if commandOp != nil {
		t.Fatal("op begun before the write gate")
	}
	CheckReadonly("dep add")
	if commandOp == nil {
		t.Fatal("CheckReadonly did not begin an op with oplog.dir set")
	}
	CheckReadonly("dep add") // a second gate call must not log a second intent
	endCommandOplog(7)
	endCommandOplog(0) // second call is a no-op

	recs := readOplog(t, dir)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	in, out := recs[0], recs[1]
	if in.Phase != oplog.PhaseIntent || in.Verb != "dep add" ||
		strings.Join(in.IDs, ",") != "bd-a1,bd-b2.3" || strings.Join(in.Flags, ",") != "note" {
		t.Fatalf("intent %+v", in)
	}
	if out.RC == nil || *out.RC != 7 || out.OpID != in.OpID {
		t.Fatalf("outcome %+v", out)
	}
	raw, _ := json.Marshal(recs)
	if strings.Contains(string(raw), "private words") {
		t.Fatal("flag value written to the op log")
	}
}

func TestCommandOplog_NoGateNoLog(t *testing.T) {
	dir := setupOplogTest(t)
	show := &cobra.Command{Use: "show"}
	stashCommandOplog(show, []string{"bd-a1"})
	endCommandOplog(0)
	if recs := readOplog(t, dir); len(recs) != 0 {
		t.Fatalf("a command that never passed the write gate logged %+v", recs)
	}
}

func TestCommandOplog_ServeExcluded(t *testing.T) {
	dir := setupOplogTest(t)
	stashCommandOplog(&cobra.Command{Use: "serve"}, nil)
	CheckReadonly("serve")
	endCommandOplog(0)
	if recs := readOplog(t, dir); len(recs) != 0 {
		t.Fatalf("serve logged %+v", recs)
	}
}

func TestCommandOplog_StashClearsPreviousOp(t *testing.T) {
	setupOplogTest(t)
	stashCommandOplog(&cobra.Command{Use: "create"}, nil)
	CheckReadonly("create")
	if commandOp == nil {
		t.Fatal("no op begun")
	}
	stashCommandOplog(&cobra.Command{Use: "list"}, nil)
	if commandOp != nil {
		t.Fatal("op from the previous command survived the stash")
	}
}

func TestCommandOplog_OffWhenDirUnset(t *testing.T) {
	setupOplogTest(t)
	config.Set("oplog.dir", "")
	stashCommandOplog(&cobra.Command{Use: "create"}, nil)
	CheckReadonly("create")
	if commandOp != nil {
		t.Fatal("op begun with oplog.dir unset")
	}
	endCommandOplog(0) // must not panic on a nil op
}

func TestOplogIssueIDs(t *testing.T) {
	long := "bd-" + strings.Repeat("a", maxOplogIDLen)
	args := []string{
		"bd-a1", "bd-a1.2.3", // kept
		"jira-token", "ATATT3x-SECRET", // hyphenated, wrong prefix
		"fix-payroll-export-for-alice", // a title
		"bd-payroll-export",            // right prefix, prose after it
		"bd-", "bd-a1.", "bd-a b",      // malformed
		long, // over the cap
		"xbd-a1",
	}
	got := oplogIssueIDs(args, "bd")
	if strings.Join(got, ",") != "bd-a1,bd-a1.2.3" {
		t.Fatalf("got %v", got)
	}
	if ids := oplogIssueIDs(args, ""); ids != nil {
		t.Fatalf("no prefix must log no ids, got %v", ids)
	}
	if ids := oplogIssueIDs([]string{"my-proj-x9k2"}, "my-proj"); strings.Join(ids, ",") != "my-proj-x9k2" {
		t.Fatalf("hyphenated prefix: got %v", ids)
	}
}
