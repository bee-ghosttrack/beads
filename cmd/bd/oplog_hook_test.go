package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/oplog"
	"github.com/steveyegge/beads/internal/storage/sqltap"
)

// setupOplogTest points oplog.dir at a temp dir and restores the package
// state afterwards.
func setupOplogTest(t *testing.T) string {
	t.Helper()
	if err := config.Initialize(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config.Set("oplog.dir", dir)
	t.Cleanup(func() {
		config.Set("oplog.dir", "")
		sqltap.Disarm()
		commandOp.Store(nil)
		oplogCmd, oplogArgs = nil, nil
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

// nopConn stands in for a Dolt connection: statements go through the tap and
// nowhere else.
type nopConn struct{}

func (nopConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (nopConn) Close() error                        { return nil }
func (nopConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (nopConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type nopConnector struct{}

func (nopConnector) Connect(context.Context) (driver.Conn, error) { return nopConn{}, nil }
func (nopConnector) Driver() driver.Driver                        { return nil }

func tappedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(sqltap.WrapConnector(nopConnector{}))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func oplogExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q); err != nil {
		t.Fatal(err)
	}
}

func TestCommandOplog_FirstWriteBeginsOnce(t *testing.T) {
	dir := setupOplogTest(t)
	db := tappedDB(t)

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

	stashCommandOplog(add, []string{"bd-a1", "op-Hunter2"})
	oplogExec(t, db, "SELECT * FROM issues")
	if commandOp.Load() != nil {
		t.Fatal("a read began an op")
	}
	CheckReadonly("dep add") // the write gate alone is not a write
	if commandOp.Load() != nil {
		t.Fatal("the write gate began an op before any write")
	}
	oplogExec(t, db, "INSERT INTO dependencies VALUES (1)")
	if commandOp.Load() == nil {
		t.Fatal("the first write did not begin an op")
	}
	oplogExec(t, db, "CALL DOLT_COMMIT('-Am', 'x')") // must not log a second intent
	endCommandOplog(7)
	endCommandOplog(0) // second call is a no-op

	recs := readOplog(t, dir)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	in, out := recs[0], recs[1]
	if in.Phase != oplog.PhaseIntent || in.Verb != "dep add" || len(in.IDs) != 0 ||
		strings.Join(in.Flags, ",") != "note" {
		t.Fatalf("intent %+v", in)
	}
	if out.RC == nil || *out.RC != 7 || out.OpID != in.OpID {
		t.Fatalf("outcome %+v", out)
	}
	raw, _ := json.Marshal(recs)
	for _, leak := range []string{"private words", "Hunter2", "bd-a1"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q written to the op log", leak)
		}
	}
}

func TestCommandOplog_NoWriteNoLog(t *testing.T) {
	dir := setupOplogTest(t)
	db := tappedDB(t)
	stashCommandOplog(&cobra.Command{Use: "delete"}, []string{"bd-a1"})
	CheckReadonly("delete") // a preview passes the gate, then only reads
	oplogExec(t, db, "SELECT id FROM issues WHERE id = 'bd-a1'")
	oplogExec(t, db, "CALL DOLT_CHECKOUT('main')")
	endCommandOplog(0)
	if recs := readOplog(t, dir); len(recs) != 0 {
		t.Fatalf("a command that never wrote logged %+v", recs)
	}
}

func TestCommandOplog_ServeExcluded(t *testing.T) {
	dir := setupOplogTest(t)
	db := tappedDB(t)
	stashCommandOplog(&cobra.Command{Use: "serve"}, nil)
	oplogExec(t, db, "INSERT INTO t VALUES (1)")
	endCommandOplog(0)
	if recs := readOplog(t, dir); len(recs) != 0 {
		t.Fatalf("serve logged %+v", recs)
	}
}

func TestCommandOplog_StashClearsAndRearms(t *testing.T) {
	dir := setupOplogTest(t)
	db := tappedDB(t)
	stashCommandOplog(&cobra.Command{Use: "create"}, nil)
	oplogExec(t, db, "INSERT INTO t VALUES (1)")
	if commandOp.Load() == nil {
		t.Fatal("no op begun")
	}
	stashCommandOplog(&cobra.Command{Use: "rename"}, nil)
	if commandOp.Load() != nil {
		t.Fatal("op from the previous command survived the stash")
	}
	oplogExec(t, db, "UPDATE t SET x = 1")
	endCommandOplog(0)
	recs := readOplog(t, dir)
	if len(recs) != 3 || recs[1].Verb != "rename" {
		t.Fatalf("second command in the process not logged: %+v", recs)
	}
}

func TestCommandOplog_OffWhenDirUnset(t *testing.T) {
	setupOplogTest(t)
	db := tappedDB(t)
	config.Set("oplog.dir", "")
	stashCommandOplog(&cobra.Command{Use: "create"}, nil)
	oplogExec(t, db, "INSERT INTO t VALUES (1)")
	if commandOp.Load() != nil {
		t.Fatal("op begun with oplog.dir unset")
	}
	endCommandOplog(0) // must not panic on a nil op
}

// The second-signal path ends the op from another goroutine while main may
// be ending it too: one outcome, no race (run with -race).
func TestCommandOplog_ConcurrentEnd(t *testing.T) {
	dir := setupOplogTest(t)
	db := tappedDB(t)
	stashCommandOplog(&cobra.Command{Use: "create"}, nil)
	oplogExec(t, db, "INSERT INTO t VALUES (1)")
	var wg sync.WaitGroup
	for _, rc := range []int{1, 0} {
		wg.Add(1)
		go func(rc int) { defer wg.Done(); endCommandOplog(rc) }(rc)
	}
	wg.Wait()
	if recs := readOplog(t, dir); len(recs) != 2 {
		t.Fatalf("want intent + one outcome, got %+v", recs)
	}
}
