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
// Classification is by the statement's leading keyword. Writes are INSERT,
// UPDATE, DELETE, REPLACE, the DDL verbs except CREATE DATABASE IF NOT EXISTS
// (which every store open sends), and CALL of any Dolt procedure except
// DOLT_CHECKOUT (a session branch switch) and DOLT_FETCH (remote-tracking refs
// only). A prepared statement is classified when it is prepared. Reads, session
// statements (SET, USE) and transaction control never fire the hook.
//
// With no hook armed the wrapper costs one atomic load per statement.
package sqltap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
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
func observe(query string) {
	if !armed.Load() || !IsWrite(query) {
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

var writeVerbs = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "RENAME": true,
}

// readOnlyProcs are Dolt procedures that move no stored data.
var readOnlyProcs = map[string]bool{"DOLT_CHECKOUT": true, "DOLT_FETCH": true}

// IsWrite reports whether query's leading statement mutates stored state.
func IsWrite(query string) bool {
	rest := skipSpaceAndComments(query)
	verb, rest := leadingWord(rest)
	verb = strings.ToUpper(verb)
	if verb == "CREATE" && isEnsureDatabase(rest) {
		return false
	}
	if writeVerbs[verb] {
		return true
	}
	if verb != "CALL" {
		return false
	}
	proc, _ := leadingWord(skipSpaceAndComments(rest))
	return !readOnlyProcs[strings.ToUpper(proc)]
}

// isEnsureDatabase reports whether rest (what follows CREATE) is
// "DATABASE|SCHEMA IF NOT EXISTS": every store open sends it, and it creates
// nothing once the database exists.
func isEnsureDatabase(rest string) bool {
	var words []string
	for len(words) < 4 {
		w, r := leadingWord(skipSpaceAndComments(rest))
		if w == "" {
			return false
		}
		words, rest = append(words, strings.ToUpper(w)), r
	}
	return (words[0] == "DATABASE" || words[0] == "SCHEMA") &&
		words[1] == "IF" && words[2] == "NOT" && words[3] == "EXISTS"
}

func skipSpaceAndComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n(")
		switch {
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s[2:], "*/")
			if end < 0 {
				return ""
			}
			s = s[2+end+2:]
		case strings.HasPrefix(s, "--"), strings.HasPrefix(s, "#"):
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				return ""
			}
			s = s[end+1:]
		default:
			return s
		}
	}
}

func leadingWord(s string) (word, rest string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
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
	observe(query)
	return c.inner.Prepare(query)
}

func (c *tapConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	observe(query)
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
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
	observe(query)
	return ec.ExecContext(ctx, query, args)
}

func (c *tapConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	observe(query)
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
