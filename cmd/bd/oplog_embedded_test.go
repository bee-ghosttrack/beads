//go:build cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/oplog"
)

// TestOplogEmbedded drives the real binary so the wiring in main.go is
// covered: the stash at the top of the root hook, the begin in CheckReadonly,
// and the outcome written after ExecuteC.
func TestOplogEmbedded(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt oplog tests")
	}
	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "op")
	logDir := t.TempDir()
	env := []string{"BD_OPLOG_DIR=" + logDir}

	const title = "fix-payroll-export-for-alice"
	out, rc := bdRunRaw(t, bd, dir, env, "create", "--silent", title)
	if rc != 0 {
		t.Fatalf("create rc %d: %s", rc, out)
	}
	id := strings.TrimSpace(out)
	recs := readOplog(t, logDir)
	if len(recs) != 2 || recs[0].Phase != oplog.PhaseIntent || recs[0].Verb != "create" ||
		recs[1].RC == nil || *recs[1].RC != 0 || recs[1].OpID != recs[0].OpID {
		t.Fatalf("create: want intent+outcome rc 0, got %+v", recs)
	}

	// Read commands never reach the write gate, whatever the store policy.
	for _, args := range [][]string{{"show", id}, {"status"}, {"list"}} {
		if out, rc := bdRunRaw(t, bd, dir, env, args...); rc != 0 {
			t.Fatalf("%v rc %d: %s", args, rc, out)
		}
	}
	if n := len(readOplog(t, logDir)); n != 2 {
		t.Fatalf("read commands logged %d new records", n-2)
	}

	if out, rc := bdRunRaw(t, bd, dir, env, "update", id, "--status", "in_progress"); rc != 0 {
		t.Fatalf("update rc %d: %s", rc, out)
	}
	if _, rc := bdRunRaw(t, bd, dir, env, "update", "op-nope9", "--status", "closed"); rc == 0 {
		t.Fatal("update of a missing issue succeeded")
	}
	recs = readOplog(t, logDir)
	if len(recs) != 6 {
		t.Fatalf("want 6 records after two updates, got %+v", recs)
	}
	if strings.Join(recs[2].IDs, ",") != id || recs[3].RC == nil || *recs[3].RC != 0 {
		t.Fatalf("update: %+v %+v", recs[2], recs[3])
	}
	if strings.Join(recs[4].IDs, ",") != "op-nope9" || recs[5].RC == nil || *recs[5].RC == 0 {
		t.Fatalf("failed update: %+v %+v", recs[4], recs[5])
	}

	files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	if strings.Contains(raw, title) || strings.Contains(raw, "in_progress") {
		t.Fatalf("payload text reached the log:\n%s", raw)
	}
}
