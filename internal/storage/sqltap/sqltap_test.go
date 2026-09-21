package sqltap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsWrite(t *testing.T) {
	writes := []string{
		"INSERT INTO issues VALUES (?)",
		"  update issues SET status = ?",
		"DELETE FROM labels",
		"REPLACE INTO config VALUES (?, ?)",
		"CREATE TABLE t (x int)", "ALTER TABLE t ADD y int", "DROP TABLE t", "TRUNCATE t",
		"/* bd */ INSERT INTO t VALUES (1)",
		"-- note\nUPDATE t SET x = 1",
		"# note\nDELETE FROM t",
		"CALL DOLT_COMMIT('-Am', 'x')", "call dolt_add('.')", "CALL DOLT_BRANCH('b')",
		"CALL DOLT_PUSH('origin', 'main')", "CALL /* c */ DOLT_RESET('--hard')",
		"(INSERT INTO t VALUES (1))",
		"CREATE DATABASE op", "CREATE DATABASE IF EXISTS_TYPO op", "DROP DATABASE IF EXISTS op",
		"CREATE TABLE IF NOT EXISTS t (x int)",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("IsWrite(%q) = false, want true", q)
		}
	}
	reads := []string{
		"SELECT * FROM issues", "select dolt_hashof('HEAD')", "SHOW TABLES",
		"SET @@autocommit = 1", "USE beads", "START TRANSACTION", "BEGIN", "COMMIT", "ROLLBACK",
		"CALL DOLT_CHECKOUT('main')", "CALL dolt_fetch('origin')",
		"WITH x AS (SELECT 1) SELECT * FROM x", "EXPLAIN UPDATE t SET x = 1",
		"", "   ", "/* unterminated", "-- only a comment",
		"INSERTED", "UPDATES", "DELETED_AT",
		"CREATE DATABASE IF NOT EXISTS `op`", "create schema /* c */ if not exists op",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("IsWrite(%q) = true, want false", q)
		}
	}
}

// fakeConn records the statements that reach the "server" and whether the
// hook had already run when each one arrived.
type fakeConn struct {
	log      *[]string
	logMu    *sync.Mutex
	hookDone *atomic.Bool
	delay    time.Duration
}

func (c *fakeConn) record(q string) {
	time.Sleep(c.delay)
	c.logMu.Lock()
	defer c.logMu.Unlock()
	mark := "before"
	if c.hookDone.Load() {
		mark = "after"
	}
	*c.log = append(*c.log, mark+":"+q)
}

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return fakeStmt{c, q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return fakeTx{}, nil }
func (c *fakeConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.record(q)
	return driver.RowsAffected(0), nil
}

type fakeStmt struct {
	c *fakeConn
	q string
}

func (s fakeStmt) Close() error  { return nil }
func (s fakeStmt) NumInput() int { return -1 }
func (s fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	s.c.record(s.q)
	return driver.RowsAffected(0), nil
}
func (s fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	s.c.record(s.q)
	return fakeRows{}, nil
}

type fakeRows struct{}

func (fakeRows) Columns() []string         { return nil }
func (fakeRows) Close() error              { return nil }
func (fakeRows) Next([]driver.Value) error { return io.EOF }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeConnector struct {
	conn   *fakeConn
	closed atomic.Bool
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, nil }
func (f *fakeConnector) Driver() driver.Driver                        { return nil }
func (f *fakeConnector) Close() error                                 { f.closed.Store(true); return nil }

func openFake(t *testing.T, delay time.Duration) (*sql.DB, *fakeConnector, *[]string, *atomic.Bool) {
	t.Helper()
	var log []string
	hookDone := &atomic.Bool{}
	fc := &fakeConnector{conn: &fakeConn{log: &log, logMu: &sync.Mutex{}, hookDone: hookDone, delay: delay}}
	db := sql.OpenDB(WrapConnector(fc))
	t.Cleanup(func() { Disarm(); _ = db.Close() })
	return db, fc, &log, hookDone
}

func TestHookFiresOnceBeforeFirstWrite(t *testing.T) {
	db, _, log, hookDone := openFake(t, 0)
	var calls int
	Arm(func() { calls++; hookDone.Store(true) })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("a read fired the hook")
	}
	for _, q := range []string{"INSERT INTO t VALUES (1)", "UPDATE t SET x = 2"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("hook ran %d times, want 1", calls)
	}
	want := "before:SELECT 1,after:INSERT INTO t VALUES (1),after:UPDATE t SET x = 2"
	if got := strings.Join(*log, ","); got != want {
		t.Fatalf("statement order %s, want %s", got, want)
	}
}

func TestHookFiresForPreparedAndTxWrites(t *testing.T) {
	db, _, _, _ := openFake(t, 0)
	ctx := context.Background()

	var calls int
	Arm(func() { calls++ })
	stmt, err := db.PrepareContext(ctx, "INSERT INTO t VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	_ = stmt.Close()
	if calls != 1 {
		t.Fatalf("prepared write: hook ran %d times", calls)
	}

	Arm(func() { calls++ })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("BEGIN fired the hook")
	}
	if _, err := tx.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'x')"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if calls != 2 {
		t.Fatalf("write inside a tx: hook ran %d times total, want 2", calls)
	}
}

func TestDisarmedAndRearm(t *testing.T) {
	db, _, _, _ := openFake(t, 0)
	ctx := context.Background()
	var calls int
	Arm(func() { calls++ })
	Disarm()
	_, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	if calls != 0 {
		t.Fatal("disarmed hook fired")
	}
	Arm(func() { calls++ })
	_, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	Arm(func() { calls++ }) // a new command in the same process re-opens the once
	_, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	if calls != 2 {
		t.Fatalf("hook ran %d times over two armings, want 2", calls)
	}
}

// A second goroutine's write must not reach the server while the hook that
// the first write triggered is still running.
func TestConcurrentWriterWaitsForHook(t *testing.T) {
	db, _, log, hookDone := openFake(t, 0)
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	release := make(chan struct{})
	entered := make(chan struct{})
	Arm(func() {
		close(entered)
		<-release
		hookDone.Store(true)
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (1)") }()
	<-entered
	go func() { defer wg.Done(); _, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (2)") }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, l := range *log {
		if strings.HasPrefix(l, "before:") {
			t.Fatalf("a write reached the server before the hook finished: %v", *log)
		}
	}
}

func TestWrappedConnectorCloses(t *testing.T) {
	db, fc, _, _ := openFake(t, 0)
	_ = db.Close()
	if !fc.closed.Load() {
		t.Fatal("DB.Close did not close the wrapped connector")
	}
}

func TestMySQLDriverRegistered(t *testing.T) {
	db, err := sql.Open(MySQLDriverName, "u:p@tcp(127.0.0.1:1)/db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok := db.Driver().(*tapDriver); !ok {
		t.Fatalf("driver is %T, want the tap", db.Driver())
	}
}

// TestNoUntappedClientOpens is the drift guard: a client-side
// sql.Open("mysql", ...) would send writes the op log never sees.
func TestNoUntappedClientOpens(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		// The proxy server's own pool: its writes are its clients' writes,
		// already seen by each client's tap.
		"internal/storage/dbproxy/server/doltserver.go": true,
	}
	var bad []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if allowed[rel] || strings.HasPrefix(rel, "internal/testutil/") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			src := string(b)
			if strings.Contains(src, `sql.Open("mysql"`) || strings.Contains(src, "sql.OpenDB(connector)") ||
				strings.Contains(src, "mysql.NewConnector(") {
				bad = append(bad, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("client SQL opened past the tap (use sqltap.MySQLDriverName / sqltap.WrapConnector): %v", bad)
	}
}
