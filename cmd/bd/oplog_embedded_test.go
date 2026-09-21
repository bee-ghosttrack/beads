//go:build cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/oplog"
)

// TestOplogEmbedded drives the real binary so the wiring is covered end to
// end: the tap arms at the top of the root hook, the intent is written at the
// first write statement, and the outcome after ExecuteC. The probes are the
// ones that broke earlier designs: previews that pass the write gate, writes
// that skip it, and arguments that must never reach the log.
func TestOplogEmbedded(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt oplog tests")
	}
	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "op")
	logDir := t.TempDir()
	env := []string{"BD_OPLOG_DIR=" + logDir}

	step := oplogStepper(t, bd, dir, env, logDir)

	const title = "fix-payroll-export-for-alice"
	out, rc := step(1, "create", "create", "--silent", title)
	if rc != 0 {
		t.Fatalf("create rc %d: %s", rc, out)
	}
	id := strings.TrimSpace(out)

	export := filepath.Join(t.TempDir(), "issues.jsonl")
	// --quiet: a shown tip is recorded with a real (dolt-ignored) write, at
	// random, and that write is rightly logged under the read verb.
	for _, args := range [][]string{{"show", id}, {"status"}, {"list"}, {"ready"}, {"export", "-o", export}} {
		if out, rc := step(0, "", append([]string{"--quiet"}, args...)...); rc != 0 {
			t.Fatalf("%v rc %d: %s", args, rc, out)
		}
	}

	// Previews pass the CLI write gate but write nothing.
	step(0, "", "delete", id)
	step(0, "", "create", "--dry-run", "preview-title-never-created")
	if _, rc := bdRunRaw(t, bd, dir, nil, "show", id); rc != 0 {
		t.Fatal("delete preview removed the issue")
	}

	if out, rc := step(1, "update", "update", id, "--status", "in_progress"); rc != 0 {
		t.Fatalf("update rc %d: %s", rc, out)
	}
	// A write that fails before it writes logs nothing.
	if _, rc := step(0, "", "update", "op-nope9", "--status", "closed"); rc == 0 {
		t.Fatal("update of a missing issue succeeded")
	}

	// Writes that never call the CLI write gate.
	if out, rc := step(1, "rename", "rename", id, "op-renamed"); rc != 0 {
		t.Fatalf("rename rc %d: %s", rc, out)
	}
	if out, rc := step(1, "kv set", "kv", "set", "op-apikey", "op-Hunter2"); rc != 0 {
		t.Fatalf("kv set rc %d: %s", rc, out)
	}
	if out, rc := step(1, "q", "q", "op-v2.0 release notes"); rc != 0 {
		t.Fatalf("q rc %d: %s", rc, out)
	}
	if out, rc := step(1, "import", "import", export); rc != 0 {
		t.Fatalf("import rc %d: %s", rc, out)
	}

	files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	for _, leak := range []string{title, "in_progress", "Hunter2", "op-apikey", "op-v2.0", "release notes", id, "op-renamed"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("%q reached the log:\n%s", leak, raw)
		}
	}
}

// oplogStepper returns a step function that runs one bd command and checks
// how many ops it logged and, when it logged one, the verb and that the
// outcome carries the command's rc.
func oplogStepper(t *testing.T, bd, dir string, env []string, logDir string) func(int, string, ...string) (string, int) {
	seen := 0
	return func(wantOps int, wantVerb string, args ...string) (string, int) {
		t.Helper()
		out, rc := bdRunRaw(t, bd, dir, env, args...)
		recs := readOplog(t, logDir)
		got := recs[seen:]
		seen = len(recs)
		if len(got) != 2*wantOps {
			t.Fatalf("%v (rc %d): want %d op(s), got records %+v\n%s", args, rc, wantOps, got, out)
		}
		if wantOps == 1 {
			in, res := got[0], got[1]
			if in.Phase != oplog.PhaseIntent || in.Verb != wantVerb || len(in.IDs) != 0 ||
				res.OpID != in.OpID || res.RC == nil || *res.RC != rc {
				t.Fatalf("%v (rc %d): intent %+v outcome %+v", args, rc, in, res)
			}
		}
		return out, rc
	}
}
