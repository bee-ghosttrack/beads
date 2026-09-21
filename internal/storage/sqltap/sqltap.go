// Package sqltap sees every SQL statement bd's client sends to Dolt, so a
// caller can act the moment a command first writes.
//
// bd has no single storage write chokepoint: transactions are opened at dozens
// of sites, and the CLI's write gate (CheckReadonly) both precedes previews
// that never write and is skipped by some writes. What every client write does
// share is a database/sql connection. This package wraps the two drivers those
// connections come from — the go-sql-driver "mysql" driver (server and
// proxied modes) and the embedded Dolt connector — and calls the armed hook
// once, synchronously, before the first mutating statement leaves the
// process.
//
// Classification (IsWriteArgs) is an allowlist of reads: a statement batch is
// a write unless every statement in it is a known read, session or
// transaction-control form. A prepared statement is classified each time it
// executes, with its bound arguments.
//
// With no hook armed the wrapper costs one atomic load per statement.
package sqltap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
)

// MySQLDriverName is the database/sql driver name for the tapped go-sql-driver
// driver. Client code opens server connections with it instead of "mysql".
const MySQLDriverName = "bd-mysql-tap"

func init() {
	sql.Register(MySQLDriverName, &tapDriver{inner: mysql.MySQLDriver{}})
}

var (
	armed atomic.Bool
	mu    sync.Mutex
	hook  func()
	fired bool
)

// Arm installs fn to run once, before the first mutating statement sent after
// this call. Arming again replaces the hook and re-opens the once.
func Arm(fn func()) {
	mu.Lock()
	defer mu.Unlock()
	hook, fired = fn, false
	armed.Store(fn != nil)
}

// Disarm removes the hook.
func Disarm() { Arm(nil) }

// observe runs the hook if query is a write and the hook has not fired yet.
// Holding mu while the hook runs makes a concurrent writer wait until the
// hook has finished, so no write leaves ahead of it.
func observe(query string, args []driver.NamedValue) {
	if !armed.Load() || !IsWriteArgs(query, args) {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if fired || hook == nil {
		return
	}
	fired = true
	// Cleared only once the hook returns: until then a concurrent writer must
	// take the slow path and wait on mu.
	defer armed.Store(false)
	hook()
}

// WrapConnector returns a connector whose connections report writes.
func WrapConnector(c driver.Connector) driver.Connector {
	return &tapConnector{inner: c}
}

type tapDriver struct{ inner driver.Driver }

func (d *tapDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &tapConn{inner: c}, nil
}

func (d *tapDriver) OpenConnector(dsn string) (driver.Connector, error) {
	if dc, ok := d.inner.(driver.DriverContext); ok {
		c, err := dc.OpenConnector(dsn)
		if err != nil {
			return nil, err
		}
		return &tapConnector{inner: c, driver: d}, nil
	}
	return &dsnConnector{dsn: dsn, driver: d}, nil
}

type dsnConnector struct {
	dsn    string
	driver *tapDriver
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c *dsnConnector) Driver() driver.Driver                        { return c.driver }

type tapConnector struct {
	inner  driver.Connector
	driver driver.Driver
}

func (c *tapConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &tapConn{inner: conn}, nil
}

func (c *tapConnector) Driver() driver.Driver {
	if c.driver != nil {
		return c.driver
	}
	return c.inner.Driver()
}

// Close releases the wrapped connector when it holds resources (the embedded
// Dolt engine does). database/sql calls it from DB.Close.
func (c *tapConnector) Close() error {
	if cl, ok := c.inner.(interface{ Close() error }); ok {
		return cl.Close()
	}
	return nil
}

// tapConn forwards every optional driver interface the inner conn has; where
// it lacks one, the fallback is what database/sql would have done itself.
type tapConn struct{ inner driver.Conn }

var (
	_ driver.Conn               = (*tapConn)(nil)
	_ driver.ConnBeginTx        = (*tapConn)(nil)
	_ driver.ConnPrepareContext = (*tapConn)(nil)
	_ driver.ExecerContext      = (*tapConn)(nil)
	_ driver.QueryerContext     = (*tapConn)(nil)
	_ driver.Pinger             = (*tapConn)(nil)
	_ driver.SessionResetter    = (*tapConn)(nil)
	_ driver.Validator          = (*tapConn)(nil)
	_ driver.NamedValueChecker  = (*tapConn)(nil)
)

func (c *tapConn) Prepare(query string) (driver.Stmt, error) {
	st, err := c.inner.Prepare(query)
	return c.wrapStmt(query, st, err)
}

func (c *tapConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		st, err := pc.PrepareContext(ctx, query)
		return c.wrapStmt(query, st, err)
	}
	st, err := c.inner.Prepare(query)
	return c.wrapStmt(query, st, err)
}

func (c *tapConn) Close() error { return c.inner.Close() }

//nolint:staticcheck // driver.Conn requires Begin; BeginTx is preferred when present.
func (c *tapConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *tapConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bc, ok := c.inner.(driver.ConnBeginTx); ok {
		return bc.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) || opts.ReadOnly {
		return nil, errors.New("sqltap: driver does not support non-default transaction options")
	}
	return c.inner.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
}

func (c *tapConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip // database/sql prepares instead, and PrepareContext observes
	}
	observe(query, args)
	return ec.ExecContext(ctx, query, args)
}

func (c *tapConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	observe(query, args)
	return qc.QueryContext(ctx, query, args)
}

func (c *tapConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *tapConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *tapConn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *tapConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.inner.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip // database/sql falls back to its default conversion
}

func (c *tapConn) wrapStmt(query string, st driver.Stmt, err error) (driver.Stmt, error) {
	if err != nil {
		return nil, err
	}
	return &tapStmt{inner: st, conn: c.inner, query: query}, nil
}

// tapStmt observes a prepared statement each time it runs: a placeholder's
// value (DOLT_CHECKOUT(?)) is only known then, and preparing sends nothing
// that mutates.
type tapStmt struct {
	inner driver.Stmt
	conn  driver.Conn // the inner connection, for its NamedValueChecker
	query string
}

var (
	_ driver.StmtExecContext   = (*tapStmt)(nil)
	_ driver.StmtQueryContext  = (*tapStmt)(nil)
	_ driver.NamedValueChecker = (*tapStmt)(nil)
)

func (s *tapStmt) Close() error  { return s.inner.Close() }
func (s *tapStmt) NumInput() int { return s.inner.NumInput() }

func namedValues(args []driver.Value) []driver.NamedValue {
	nv := make([]driver.NamedValue, len(args))
	for i, v := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return nv
}

//nolint:staticcheck // driver.Stmt requires Exec; ExecContext is preferred when present.
func (s *tapStmt) Exec(args []driver.Value) (driver.Result, error) {
	observe(s.query, namedValues(args))
	return s.inner.Exec(args)
}

//nolint:staticcheck // driver.Stmt requires Query; QueryContext is preferred when present.
func (s *tapStmt) Query(args []driver.Value) (driver.Rows, error) {
	observe(s.query, namedValues(args))
	return s.inner.Query(args)
}

func (s *tapStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	observe(s.query, args)
	if ec, ok := s.inner.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	vals, err := plainValues(args)
	if err != nil {
		return nil, err
	}
	return s.inner.Exec(vals) //nolint:staticcheck // fallback for statements without ExecContext
}

func (s *tapStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	observe(s.query, args)
	if qc, ok := s.inner.(driver.StmtQueryContext); ok {
		return qc.QueryContext(ctx, args)
	}
	vals, err := plainValues(args)
	if err != nil {
		return nil, err
	}
	return s.inner.Query(vals) //nolint:staticcheck // fallback for statements without QueryContext
}

// plainValues mirrors database/sql's own fallback for a statement without the
// context methods: named parameters cannot be passed through.
func plainValues(named []driver.NamedValue) ([]driver.Value, error) {
	vals := make([]driver.Value, len(named))
	for i, nv := range named {
		if nv.Name != "" {
			return nil, errors.New("sqltap: driver does not support the use of Named Parameters")
		}
		vals[i] = nv.Value
	}
	return vals, nil
}

// CheckNamedValue follows database/sql's own order — the statement's checker,
// then the connection's — so arguments convert exactly as they would
// unwrapped. A statement-level ColumnConverter is not consulted: neither
// wrapped driver has one without a checker (go-sql-driver's statement has
// both; the embedded driver's has neither), and forwarding it would change
// the fallback for drivers whose NumInput is -1.
func (s *tapStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := s.inner.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	if n, ok := s.conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
