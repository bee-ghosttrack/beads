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

func TestCommandOplog_IntentAndOutcome(t *testing.T) {
	if err := config.Initialize(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config.Set("oplog.dir", dir)
	t.Cleanup(func() { config.Set("oplog.dir", ""); commandOp = nil })

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

	beginCommandOplog(add, []string{"bd-a1", "bd-b2", "not an id"})
	if commandOp == nil {
		t.Fatal("no op begun with oplog.dir set")
	}
	endCommandOplog(7)
	endCommandOplog(0) // second call is a no-op

	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want one log file, got %v", matches)
	}
	raw, _ := os.ReadFile(matches[0])
	if strings.Contains(string(raw), "private words") {
		t.Fatal("flag value written to the op log")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 records, got %d", len(lines))
	}
	var in, out oplog.Record
	_ = json.Unmarshal([]byte(lines[0]), &in)
	_ = json.Unmarshal([]byte(lines[1]), &out)
	if in.Verb != "dep add" || strings.Join(in.IDs, ",") != "bd-a1,bd-b2" || strings.Join(in.Flags, ",") != "note" {
		t.Fatalf("intent %+v", in)
	}
	if out.RC == nil || *out.RC != 7 || out.OpID != in.OpID {
		t.Fatalf("outcome %+v", out)
	}
}

func TestCommandOplog_OffWhenDirUnset(t *testing.T) {
	if err := config.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Set("oplog.dir", "")
	cmd := &cobra.Command{Use: "create"}
	beginCommandOplog(cmd, nil)
	if commandOp != nil {
		t.Fatal("op begun with oplog.dir unset")
	}
	endCommandOplog(0) // must not panic on a nil op
}
