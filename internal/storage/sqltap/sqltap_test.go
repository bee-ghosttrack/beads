package sqltap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The MySQL lexing keeps non-ASCII bytes inside a word and a backslash
// inside backticks; the dual and vitess lexings would mask a slip here.
func TestTokenizeMySQL(t *testing.T) {
	for _, backslash := range []bool{true, false} {
		toks := tokenize("SELECT t\u00fcinsert, `a\\` FROM t", backslash)
		want := []token{{tWord, "SELECT"}, {tWord, "T\u00dcINSERT"}, {tPunct, ","},
			{tIdent, "a\\"}, {tWord, "FROM"}, {tWord, "T"}}
		if len(toks) != len(want) {
			t.Fatalf("tokenize(backslash=%v) = %v, want %v", backslash, toks, want)
		}
		for i := range want {
			if toks[i] != want[i] {
				t.Errorf("tokenize(backslash=%v)[%d] = %v, want %v", backslash, i, toks[i], want[i])
			}
		}
	}
}

func TestIsWrite(t *testing.T) {
	writes := []string{
		"INSERT INTO issues VALUES (?)",
		"  update issues SET status = ?",
		"DELETE FROM labels",
		"REPLACE INTO config VALUES (?, ?)",
		"CREATE TABLE t (x int)", "ALTER TABLE t ADD y int", "DROP TABLE t", "TRUNCATE t",
		"RENAME TABLE a TO b",
		"/* bd */ INSERT INTO t VALUES (1)",
		"-- note\nUPDATE t SET x = 1",
		"# note\nDELETE FROM t",
		"CALL DOLT_COMMIT('-Am', 'x')", "call dolt_add('.')", "CALL DOLT_BRANCH('b')",
		"CALL DOLT_PUSH('origin', 'main')", "CALL /* c */ DOLT_RESET('--hard')",
		"CALL dolt_fetch('origin')", "CALL my_proc()", "CALL `dolt_checkout`('main')",
		"(INSERT INTO t VALUES (1))",
		"CREATE DATABASE op", "CREATE DATABASE IF EXISTS_TYPO op", "DROP DATABASE IF EXISTS op",
		"CREATE TABLE IF NOT EXISTS t (x int)",
		// The round-3 review's live probes: each wrote with no op logged.
		"SELECT 1; INSERT INTO t VALUES (1)",
		"/*!40000 INSERT INTO t VALUES (1) */",
		"/*! DELETE FROM t */",
		"WITH x AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM x)",
		"WITH x AS (SELECT 1) UPDATE t SET y = 1",
		"CALL DOLT_CHECKOUT('-b', 'newbranch')",
		"CALL DOLT_CHECKOUT('-b')",
		"CALL DOLT_CHECKOUT('--', 't')",
		"CALL DOLT_CHECKOUT('')",
		"LOAD DATA INFILE '/tmp/x' INTO TABLE t",
		"GRANT ALL ON *.* TO u", "REVOKE ALL ON *.* FROM u",
		"SET PERSIST max_connections = 10", "SET GLOBAL max_connections = 10",
		"SET @@global.max_connections = 10", "SET @@PERSIST.x = 1", "SET PERSIST_ONLY x = 1",
		"SET @a = 1, GLOBAL max_connections = 10",
		"SET PASSWORD = 'x'", "SET DEFAULT ROLE r TO u", "SET RESOURCE GROUP g",
		"PREPARE s FROM 'DELETE FROM t'", "EXECUTE s", "DEALLOCATE PREPARE s",
		"CREATE DATABASE IF NOT EXISTS x; DROP TABLE t",
		"CREATE DATABASE IF NOT EXISTS x CHARACTER SET utf8mb4",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'", "SELECT 1 INTO @v",
		"EXPLAIN ANALYZE DELETE FROM t", "EXPLAIN ANALYZE UPDATE t SET x = 1",
		"DO RELEASE_LOCK('x')", "LOCK TABLES t WRITE", "ANALYZE TABLE t", "FLUSH TABLES",
		// Statement boundaries survive quoting and comments that hide them.
		"SELECT ';'; INSERT INTO t VALUES (1)",
		"SELECT 'it''s'; DELETE FROM t",
		`SELECT 'a\'; DELETE FROM t'; UPDATE t SET x = 1`,
		"SELECT \"x\"; DROP TABLE t",
		"SELECT `a;b` FROM t; DELETE FROM t",
		"SELECT 1 /* ; */; INSERT INTO t VALUES (1)",
		"SELECT 1 -- c\n; INSERT INTO t VALUES (1)",
		`SELECT 'a\'; SELECT 1'; DELETE FROM t`,           // escaped quote: the string ends later
		`SELECT 'a\'; INSERT INTO t VALUES (1); SELECT '`, // ...unless NO_BACKSLASH_ESCAPES
		`CALL DOLT_CHECKOUT('o\'brien')`,                  // not one argument without backslash escapes
		"SELECT 1--1; INSERT INTO t VALUES (1)",           // --1 is not a comment
		"SELECT /*+ SET_VAR(x=1) */ 1; DELETE FROM t",
		"INSERTED", "UPDATES", "DELETED_AT", ";;;x", "?",
		// The round-4 review's live probes: Dolt's vitess tokenizer lexes
		// these differently from MySQL, and each hid a DELETE in server mode.
		"SELECT 1; /*M! DELETE FROM t */",               // (a) MariaDB executable comment
		"SELECT 1 /*! , 1 # */ ; DELETE FROM t",         // (b) /*! ends at the first */
		"SELECT 1 --x '\n; DELETE FROM t",               // (c) any -- is a comment
		"SELECT 1 // '\n; DELETE FROM t",                // (d) // is a comment
		"SELECT 1 /*+ ' */ ; DELETE FROM t; SELECT 'x'", // (e) /*+ is a plain comment
		"SELECT /*+ ; DELETE FROM t; */ 1",              // ...but MySQL reads it as SQL
		"/* unterminated",                               // vitess cannot lex it
		"START REPLICA", "START SLAVE", "RELEASE x", "RELEASE_LOCK('x')",
		"SET @@persist_only.x = 1",
		// DOLT_CHECKOUT of a literal outside bd's branches may name a table.
		"CALL DOLT_CHECKOUT('probe')", "call dolt_checkout(\"feature/x\")",
		"CALL DOLT_CHECKOUT('o''brien')", "CALL DOLT_CHECKOUT('Main')",
		"call dolt_checkout(\"main\")", // under ANSI_QUOTES, an identifier
		"// only a comment",            // MySQL does not take // as a comment
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("IsWrite(%q) = false, want true", q)
		}
	}
	reads := []string{
		"SELECT * FROM issues", "select dolt_hashof('HEAD')", "SHOW TABLES",
		"SET @@autocommit = 1", "SET NAMES utf8mb4", "SET SESSION x = 1", "SET @@session.x = 1",
		"SET @v = 'GLOBAL'", "SET TRANSACTION ISOLATION LEVEL READ COMMITTED",
		"USE beads", "START TRANSACTION", "BEGIN", "COMMIT", "ROLLBACK",
		"SAVEPOINT s", "RELEASE SAVEPOINT s", "ROLLBACK TO SAVEPOINT s",
		"CALL DOLT_CHECKOUT('main')",
		"CALL DOLT_CHECKOUT('flatten-tmp')", "CALL DOLT_CHECKOUT('compact-tmp')",
		"WITH x AS (SELECT 1) SELECT * FROM x", "WITH RECURSIVE r AS (SELECT 1) SELECT * FROM r",
		"EXPLAIN UPDATE t SET x = 1", "EXPLAIN SELECT 1", "DESCRIBE t", "DESC t",
		"EXPLAIN ANALYZE SELECT * FROM t",
		"SELECT * FROM t WHERE id = ? FOR UPDATE", "SELECT 'INSERT INTO t'", "SELECT `delete` FROM t",
		"(SELECT 1) UNION (SELECT 2)", "TABLE t", "VALUES ROW(1)", "HELP 'x'",
		"SELECT * FROM dolt_diff('HEAD~1', 'HEAD', 'issues')",
		"", "   ", ";", "SELECT 1;", "SELECT 1; SELECT 2", "-- only a comment",
		"SELECT `into` FROM t", "SELECT \"x\" FROM t", "SELECT 1 /*M! , 2 */",
		"/*! SELECT 1 */", "/*!40000 SET NAMES utf8mb4 */",
		"/*!32312 CREATE DATABASE IF NOT EXISTS op */", // mysqldump's form
		"CREATE DATABASE IF NOT EXISTS `op`", "create schema /* c */ if not exists op",
		"CREATE DATABASE IF NOT EXISTS op;",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("IsWrite(%q) = true, want false", q)
		}
	}
}

// A DOLT_CHECKOUT placeholder is judged by its bound value.
func TestIsWriteArgsCheckoutPlaceholder(t *testing.T) {
	arg := func(v driver.Value) []driver.NamedValue { return []driver.NamedValue{{Ordinal: 1, Value: v}} }
	cases := []struct {
		args  []driver.NamedValue
		write bool
	}{
		{arg("main"), false},
		{arg("-b"), true},
		{arg(""), true},
		{arg([]byte("main")), true}, // not a string: unknown
		{nil, true},                 // unbound: unknown
		{append(arg("main"), driver.NamedValue{Ordinal: 2, Value: "x"}), true},
	}
	for _, c := range cases {
		if got := IsWriteArgs("CALL DOLT_CHECKOUT(?)", c.args); got != c.write {
			t.Errorf("IsWriteArgs(CALL DOLT_CHECKOUT(?), %v) = %v, want %v", c.args, got, c.write)
		}
	}
	if !IsWriteArgs("CALL DOLT_CHECKOUT(?, ?)", arg("main")) {
		t.Error("two placeholders judged as a branch switch")
	}
}

// fakeConn records the statements that reach the "server" and whether the
// hook had already run when each one arrived. It has the context fast paths
// (ExecerContext, QueryerContext) that go-sql-driver and the embedded driver
// both have.
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
func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.record(q)
	return fakeRows{}, nil
}

// bareConn has only the required driver.Conn methods, so database/sql must
// prepare every statement and fall back on every optional interface.
type bareConn struct{ fc *fakeConn }

func (c bareConn) Prepare(q string) (driver.Stmt, error) { return fakeStmt{c.fc, q}, nil }
func (c bareConn) Close() error                          { return nil }
func (c bareConn) Begin() (driver.Tx, error)             { return fakeTx{}, nil }

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
	conn   driver.Conn
	closed atomic.Bool
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, nil }
func (f *fakeConnector) Driver() driver.Driver                        { return nil }
func (f *fakeConnector) Close() error                                 { f.closed.Store(true); return nil }

func newFakeConn() *fakeConn {
	return &fakeConn{log: &[]string{}, logMu: &sync.Mutex{}, hookDone: &atomic.Bool{}}
}

func openConn(t *testing.T, conn driver.Conn) (*sql.DB, *fakeConnector) {
	t.Helper()
	fc := &fakeConnector{conn: conn}
	db := sql.OpenDB(WrapConnector(fc))
	t.Cleanup(func() { Disarm(); _ = db.Close() })
	return db, fc
}

func openFake(t *testing.T) (*sql.DB, *fakeConnector, *[]string, *atomic.Bool) {
	t.Helper()
	c := newFakeConn()
	db, fc := openConn(t, c)
	return db, fc, c.log, c.hookDone
}

func TestHookFiresOnceBeforeFirstWrite(t *testing.T) {
	db, _, log, hookDone := openFake(t)
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

// The QueryContext fast path is a live write path: schema.DrainCall sends
// CALL DOLT_COMMIT/ADD/RESET through it.
func TestHookFiresOnQueryPath(t *testing.T) {
	db, _, log, hookDone := openFake(t)
	var calls int
	Arm(func() { calls++; hookDone.Store(true) })
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if calls != 0 {
		t.Fatal("a read query fired the hook")
	}
	rows, err = db.QueryContext(ctx, "CALL DOLT_COMMIT('-Am', 'x')")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if calls != 1 {
		t.Fatalf("a write sent as a query: hook ran %d times, want 1", calls)
	}
	if got := strings.Join(*log, ","); got != "before:SELECT 1,after:CALL DOLT_COMMIT('-Am', 'x')" {
		t.Fatalf("statement order %s", got)
	}
}

// A prepared statement is judged when it runs, with its arguments — not when
// it is prepared, which sends nothing that mutates.
func TestPreparedStatementsJudgedAtExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn func(*fakeConn) driver.Conn
	}{
		{"prepare-context", func(c *fakeConn) driver.Conn { return c }},
		{"bare", func(c *fakeConn) driver.Conn { return bareConn{c} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConn()
			db, _ := openConn(t, tc.conn(fc))
			ctx := context.Background()
			var calls int
			Arm(func() { calls++; fc.hookDone.Store(true) })

			stmt, err := db.PrepareContext(ctx, "INSERT INTO t VALUES (?)")
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("preparing a write fired the hook")
			}
			if _, err := stmt.ExecContext(ctx, 1); err != nil {
				t.Fatal(err)
			}
			_ = stmt.Close()
			if calls != 1 {
				t.Fatalf("executing a prepared write: hook ran %d times, want 1", calls)
			}

			Arm(func() { calls++ })
			co, err := db.PrepareContext(ctx, "CALL DOLT_CHECKOUT(?)")
			if err != nil {
				t.Fatal(err)
			}
			rows, err := co.QueryContext(ctx, "main")
			if err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			if calls != 1 {
				t.Fatal("a prepared branch switch fired the hook")
			}
			rows, err = co.QueryContext(ctx, "-b")
			if err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			_ = co.Close()
			if calls != 2 {
				t.Fatalf("a prepared checkout -b: hook ran %d times total, want 2", calls)
			}

			// With no ExecerContext on the conn, database/sql prepares a
			// one-shot Exec too; it must still be seen.
			Arm(func() { calls++ })
			if _, err := db.ExecContext(ctx, "DELETE FROM t"); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("one-shot write: hook ran %d times total, want 3", calls)
			}
		})
	}
}

func TestHookFiresInsideTx(t *testing.T) {
	db, _, _, _ := openFake(t)
	ctx := context.Background()
	var calls int
	Arm(func() { calls++ })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("BEGIN fired the hook")
	}
	if _, err := tx.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'x')"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if calls != 1 {
		t.Fatalf("write inside a tx: hook ran %d times, want 1", calls)
	}
}

func TestDisarmedAndRearm(t *testing.T) {
	db, _, _, _ := openFake(t)
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
	db, _, log, hookDone := openFake(t)
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

// fullConn implements every optional interface tapConn forwards, each
// returning a value the fallback would not, so a dropped forward shows.
type fullConn struct {
	bareConn
	opts    driver.TxOptions
	reset   bool
	checked bool
}

var errFull = errors.New("from the inner conn")

func (c *fullConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	c.opts = o
	return fakeTx{}, nil
}
func (c *fullConn) Ping(context.Context) error         { return errFull }
func (c *fullConn) ResetSession(context.Context) error { c.reset = true; return errFull }
func (c *fullConn) IsValid() bool                      { return false }
func (c *fullConn) CheckNamedValue(*driver.NamedValue) error {
	c.checked = true
	return nil
}
func (c *fullConn) PrepareContext(_ context.Context, q string) (driver.Stmt, error) {
	return checkedStmt{fakeStmt{c.fc, q}}, nil
}

// checkedStmt has its own NamedValueChecker, as go-sql-driver's does.
type checkedStmt struct{ fakeStmt }

func (checkedStmt) CheckNamedValue(*driver.NamedValue) error { return driver.ErrRemoveArgument }

func TestTapConnForwardsOptionalInterfaces(t *testing.T) {
	ctx := context.Background()
	inner := &fullConn{bareConn: bareConn{newFakeConn()}}
	c := &tapConn{inner: inner}

	want := driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelSerializable), ReadOnly: true}
	if _, err := c.BeginTx(ctx, want); err != nil || inner.opts != want {
		t.Errorf("BeginTx: err %v, inner saw %+v, want %+v", err, inner.opts, want)
	}
	if err := c.Ping(ctx); !errors.Is(err, errFull) {
		t.Errorf("Ping = %v, want the inner error", err)
	}
	if err := c.ResetSession(ctx); !errors.Is(err, errFull) || !inner.reset {
		t.Errorf("ResetSession = %v (inner called %v)", err, inner.reset)
	}
	if c.IsValid() {
		t.Error("IsValid = true, want the inner false")
	}
	if err := c.CheckNamedValue(&driver.NamedValue{}); err != nil || !inner.checked {
		t.Errorf("CheckNamedValue = %v (inner called %v), want the inner nil", err, inner.checked)
	}
	st, err := c.PrepareContext(ctx, "SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{}); !errors.Is(err, driver.ErrRemoveArgument) {
		t.Errorf("statement CheckNamedValue = %v, want the inner statement's", err)
	}
}

func TestTapConnFallbacks(t *testing.T) {
	ctx := context.Background()
	c := &tapConn{inner: bareConn{newFakeConn()}}

	if _, err := c.ExecContext(ctx, "SELECT 1", nil); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("ExecContext = %v, want ErrSkip", err)
	}
	if _, err := c.QueryContext(ctx, "SELECT 1", nil); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("QueryContext = %v, want ErrSkip", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping = %v, want nil", err)
	}
	if err := c.ResetSession(ctx); err != nil {
		t.Errorf("ResetSession = %v, want nil", err)
	}
	if !c.IsValid() {
		t.Error("IsValid = false, want true")
	}
	if err := c.CheckNamedValue(&driver.NamedValue{}); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("CheckNamedValue = %v, want ErrSkip (database/sql's default conversion)", err)
	}
	if _, err := c.BeginTx(ctx, driver.TxOptions{}); err != nil {
		t.Errorf("BeginTx default options: %v", err)
	}
	if _, err := c.BeginTx(ctx, driver.TxOptions{ReadOnly: true}); err == nil {
		t.Error("BeginTx read-only on a driver without BeginTx: want an error, as database/sql gives")
	}
	st, err := c.PrepareContext(ctx, "SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{}); !errors.Is(err, driver.ErrSkip) {
		t.Errorf("statement CheckNamedValue = %v, want ErrSkip", err)
	}

	// A statement with no checker defers to its connection's, as
	// database/sql would unwrapped.
	full := &fullConn{bareConn: bareConn{newFakeConn()}}
	fc := &tapConn{inner: full}
	st, _ = fc.Prepare("SELECT ?")
	if err := st.(driver.NamedValueChecker).CheckNamedValue(&driver.NamedValue{}); err != nil || !full.checked {
		t.Errorf("statement CheckNamedValue = %v (conn called %v), want the conn's", err, full.checked)
	}
}

// database/sql always prefers the context methods, so the legacy Stmt.Exec and
// Stmt.Query are reached only by a caller holding the driver.Stmt itself.
func TestLegacyStmtMethodsObserve(t *testing.T) {
	fc := newFakeConn()
	c := &tapConn{inner: bareConn{fc}}
	var calls int
	Arm(func() { calls++ })
	t.Cleanup(Disarm)
	st, err := c.Prepare("CALL DOLT_CHECKOUT(?)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Query([]driver.Value{"main"}); err != nil { //nolint:staticcheck // the legacy path under test
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("legacy Query of a branch switch fired the hook")
	}
	if _, err := st.Exec([]driver.Value{"-b"}); err != nil { //nolint:staticcheck // the legacy path under test
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("legacy Exec of checkout -b: hook ran %d times, want 1", calls)
	}
	Arm(func() { calls++ })
	st, _ = c.Prepare("DELETE FROM t")
	if _, err := st.Query(nil); err != nil { //nolint:staticcheck // the legacy path under test
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("legacy Query of a write: hook ran %d times total, want 2", calls)
	}
}

func TestWrappedConnectorCloses(t *testing.T) {
	db, fc, _, _ := openFake(t)
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

var (
	sqlOpenRe   = regexp.MustCompile(`\bsql\.Open\(\s*([^,]*),`)
	sqlOpenDBRe = regexp.MustCompile(`\bsql\.OpenDB\(\s*([^)]*)`)
	connectorRe = regexp.MustCompile(`\bmysql\.NewConnector\(`)
)

// untappedOpens returns why each client SQL open in src bypasses the tap.
func untappedOpens(src string) []string {
	var bad []string
	for _, m := range sqlOpenRe.FindAllStringSubmatch(src, -1) {
		if strings.TrimSpace(m[1]) != "sqltap.MySQLDriverName" {
			bad = append(bad, "sql.Open("+m[1])
		}
	}
	for _, m := range sqlOpenDBRe.FindAllStringSubmatch(src, -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[1]), "sqltap.WrapConnector(") {
			bad = append(bad, "sql.OpenDB("+m[1])
		}
	}
	if connectorRe.MatchString(src) {
		bad = append(bad, "mysql.NewConnector(")
	}
	return bad
}

func TestUntappedOpensMatcher(t *testing.T) {
	for src, want := range map[string]int{
		`sql.Open(sqltap.MySQLDriverName, dsn)`:                     0,
		`sql.Open( sqltap.MySQLDriverName , dsn)`:                   0,
		`sql.OpenDB(sqltap.WrapConnector(connector))`:               0,
		`sql.Open("mysql", dsn)`:                                    1,
		"sql.Open(\n\t\"mysql\",\n\tdsn)":                           1,
		`sql.Open(driverName, dsn)`:                                 1,
		`sql.OpenDB(connector)`:                                     1,
		`sql.OpenDB(mysql.NewConnector(cfg))`:                       2,
		`c, _ := mysql.NewConnector(cfg); sql.OpenDB(c)`:            2,
		`sql.Open(sqltap.MySQLDriverName, a); sql.Open("mysql", b)`: 1,
	} {
		if got := untappedOpens(src); len(got) != want {
			t.Errorf("untappedOpens(%q) = %v, want %d findings", src, got, want)
		}
	}
}

// TestNoUntappedClientOpens is the drift guard: a client SQL connection
// opened past the tap would send writes the op log never sees.
func TestNoUntappedClientOpens(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]string{
		"internal/storage/dbproxy/server/doltserver.go": "the proxy server's own pool: its writes are its clients' writes, already seen by each client's tap",
		"internal/migration/legacysqlite/reader.go":     "a read-only SQLite source, not Dolt",
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
			if allowed[rel] != "" || strings.HasPrefix(rel, "internal/testutil/") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, why := range untappedOpens(string(b)) {
				bad = append(bad, rel+": "+why)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("client SQL opened past the tap (use sqltap.MySQLDriverName / sqltap.WrapConnector):\n%s",
			strings.Join(bad, "\n"))
	}
}
