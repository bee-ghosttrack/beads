//go:build cgo

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOplogServerSQLProbes drives bd sql against a real dolt sql-server with
// the statements that slipped past the leading-keyword classifier: server
// DSNs allow multi-statement batches, MySQL executes /*! ... */ comments, a
// CTE can lead a DELETE, and DOLT_CHECKOUT -b creates a branch. Each must log
// one op; reads must log none.
func TestOplogServerSQLProbes(t *testing.T) {
	if os.Getenv("BEADS_TEST_OPLOG_SERVER") != "1" {
		t.Skip("set BEADS_TEST_OPLOG_SERVER=1 (needs dolt on PATH) to run the server-mode oplog probes")
	}
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not on PATH")
	}
	port := startOplogDoltServer(t, doltBin)

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "op", "--server",
		"--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port), "--server-user", "root")
	logDir := t.TempDir()
	step := oplogStepper(t, bd, dir, []string{"BD_OPLOG_DIR=" + logDir}, logDir)

	sql := func(wantOps int, q string) string {
		t.Helper()
		out, rc := step(wantOps, "sql", "--quiet", "sql", q)
		if rc != 0 {
			t.Fatalf("bd sql %q rc %d: %s", q, rc, out)
		}
		return out
	}
	sql(1, "CREATE TABLE oplog_probe (x int primary key)")
	for _, q := range []string{
		"SELECT 1; INSERT INTO oplog_probe VALUES (1)",
		"/*!40000 INSERT INTO oplog_probe VALUES (2) */",
		"WITH d AS (SELECT 1 AS v) DELETE FROM oplog_probe WHERE x IN (SELECT v FROM d)",
		"CALL DOLT_CHECKOUT('-b', 'oplog-probe-branch')",
	} {
		sql(1, q)
	}
	for _, q := range []string{"SELECT x FROM oplog_probe", "SHOW TABLES", "SELECT name FROM dolt_branches"} {
		sql(0, q)
	}
	// The probes wrote what they claimed: row 1 inserted then deleted, row 2
	// inserted through the executable comment, the branch created.
	if out := sql(0, "SELECT GROUP_CONCAT(x) AS xs FROM oplog_probe"); !strings.Contains(out, "2") || strings.Contains(out, "1,") {
		t.Fatalf("probe rows: %s", out)
	}
	if out := sql(0, "SELECT name FROM dolt_branches"); !strings.Contains(out, "oplog-probe-branch") {
		t.Fatalf("checkout -b made no branch: %s", out)
	}

	// The round-4 review's probes: Dolt's vitess tokenizer draws these
	// comment and statement boundaries unlike MySQL, and each DELETE ran with
	// no op logged.
	sql(1, "INSERT INTO oplog_probe VALUES (10), (11), (12), (13), (14)")
	for _, q := range []string{
		"SELECT 1; /*M! DELETE FROM oplog_probe WHERE x = 10 */",
		"SELECT 1 /*! , 1 # */ ; DELETE FROM oplog_probe WHERE x = 11",
		"SELECT 1 --x '\n; DELETE FROM oplog_probe WHERE x = 12",
		"SELECT 1 // '\n; DELETE FROM oplog_probe WHERE x = 13",
		"SELECT 1 /*+ ' */ ; DELETE FROM oplog_probe WHERE x = 14; SELECT 'x'",
	} {
		sql(1, q)
	}
	if out := sql(0, "SELECT GROUP_CONCAT(x) AS xs FROM oplog_probe WHERE x >= 10"); !strings.Contains(out, "<nil>") {
		t.Fatalf("a vitess-lexed DELETE did not run: %s", out)
	}
	t.Logf("vitess probes deleted rows 10-14; checkout next")
	// DOLT_CHECKOUT of a table name resets that table's working set.
	sql(1, "CALL DOLT_CHECKOUT('oplog_probe')")
}

// startOplogDoltServer runs a private dolt sql-server for one test and stops
// the process it started when the test ends.
func startOplogDoltServer(t *testing.T, doltBin string) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	data := t.TempDir()
	home := t.TempDir()
	cfg := exec.Command(doltBin, "config", "--global", "--add", "user.name", "oplog-test")
	cfg.Env = append(os.Environ(), "HOME="+home, "DOLT_ROOT_PATH="+home)
	if out, err := cfg.CombinedOutput(); err != nil {
		t.Fatalf("dolt config: %v\n%s", err, out)
	}
	cfg = exec.Command(doltBin, "config", "--global", "--add", "user.email", "oplog-test@example.com")
	cfg.Env = append(os.Environ(), "HOME="+home, "DOLT_ROOT_PATH="+home)
	if out, err := cfg.CombinedOutput(); err != nil {
		t.Fatalf("dolt config: %v\n%s", err, out)
	}

	srv := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", data)
	srv.Env = append(os.Environ(), "HOME="+home, "DOLT_ROOT_PATH="+home)
	logf, err := os.Create(filepath.Join(home, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Stdout, srv.Stderr = logf, logf
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
		_ = logf.Close()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			_ = c.Close()
			return port
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(home, "server.log"))
			t.Fatalf("dolt sql-server did not listen on %d: %v\n%s", port, err, b)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
