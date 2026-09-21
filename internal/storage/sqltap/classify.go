package sqltap

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"unicode"

	"github.com/dolthub/vitess/go/vt/sqlparser"
)

// IsWrite reports whether query may mutate stored state. It is IsWriteArgs
// with no bound arguments.
func IsWrite(query string) bool { return IsWriteArgs(query, nil) }

// IsWriteArgs reports whether query may mutate stored state, given the
// arguments bound to its ? placeholders (nil when they are not yet known).
//
// Classification is an allowlist of reads: query is split into statements on
// top-level semicolons, and it is a write unless EVERY statement is one of
//
//   - SELECT, TABLE, VALUES, WITH or SHOW with no INTO clause and no
//     INSERT/UPDATE/DELETE/REPLACE keyword (FOR UPDATE aside);
//   - EXPLAIN, DESCRIBE, DESC — but EXPLAIN ANALYZE runs its statement, so
//     it is judged by the same keywords;
//   - USE, and a session-scoped SET (not GLOBAL, PERSIST, PERSIST_ONLY,
//     PASSWORD, DEFAULT ROLE or RESOURCE GROUP);
//   - transaction control: BEGIN, START TRANSACTION, COMMIT, ROLLBACK,
//     SAVEPOINT, RELEASE SAVEPOINT;
//   - CREATE DATABASE|SCHEMA IF NOT EXISTS <name>, which every store open
//     sends and which creates nothing once the database exists. A first
//     create goes unlogged: the client cannot tell it from the no-op, and
//     bd init's create already runs inside a logged write command;
//   - CALL DOLT_CHECKOUT(<branch>): a session branch switch, where <branch>
//     is one of bd's literal branches (main, flatten-tmp, compact-tmp) or a
//     ? bound to a non-flag string. Dolt gives a table name the same syntax
//     (it resets that table's working set); a literal outside the list is
//     therefore a write, and a bound value is trusted to be a branch, as bd
//     only ever binds one.
//
// Anything else, including any statement this cannot parse, is a write.
//
// The query is lexed several ways and a write under ANY of them counts. Dolt
// splits and parses with its vitess tokenizer, which differs from MySQL's on
// comments (it executes MariaDB /*M! ... */, ends /*! at the first */ even
// after a #, takes any -- and // as a line comment, and skips /*+ ... */), so
// the query is lexed by vitess itself, with and without ANSI_QUOTES; a vitess
// lex error is a write. It is also lexed MySQL's way — /*! and /*+ bodies
// read as SQL — with and without backslash escapes in quoted text.
//
// Dolt does not split a multi-statement query in one pass, though: it parses
// the first statement, then re-parses the remainder with a fresh tokenizer. A
// ; inside /*! ... */ can end statement 1, and the remainder then starts in
// the middle of that comment, where its */ is no longer a close. So each
// piece Dolt would execute is found by running Dolt's own loop, with and
// without ANSI_QUOTES, and classified the same several ways. A piece Dolt
// cannot parse is a write. The server re-reads the session's sql_mode before
// each piece, so a SET earlier in the query can flip ANSI_QUOTES for the rest;
// every piece is therefore parsed in both modes, whatever mode split the
// pieces before it.
func IsWriteArgs(query string, args []driver.NamedValue) bool {
	if lexedWrite(query, args) {
		return true
	}
	pieces, ok := doltPieces(query)
	if !ok {
		return true
	}
	for _, piece := range pieces {
		if piece != query && lexedWrite(piece, args) {
			return true
		}
	}
	return false
}

// doltPieces returns every statement Dolt may execute for query, by the loop
// its server and embedded driver run (go-mysql-server MysqlParser.Parse in
// multi mode): parse one statement, then re-parse what follows it. The server
// loads sql_mode afresh for each parse, so each remainder is parsed with and
// without ANSI_QUOTES regardless of the mode that produced it; the walk is
// memoized on (offset, mode), so it stays linear in the number of pieces. ok
// is false when any piece does not parse in a mode that can reach it.
func doltPieces(query string) (pieces []string, ok bool) {
	type state struct {
		off  int
		ansi bool
	}
	seen := map[state]bool{}
	found := map[string]bool{}
	var walk func(off int) bool
	walk = func(off int) bool {
		for _, ansi := range []bool{false, true} {
			st := state{off, ansi}
			if seen[st] {
				continue
			}
			seen[st] = true
			piece, next, ok := doltPiece(query[off:], ansi)
			if !ok {
				return false
			}
			if piece != "" && !found[piece] {
				found[piece] = true
				pieces = append(pieces, piece)
			}
			if next > 0 && !walk(off+next) {
				return false
			}
		}
		return true
	}
	if !walk(0) {
		return nil, false
	}
	return pieces, true
}

// doltPiece parses the first statement Dolt would execute from s. It returns
// that statement ("" when only comments remain), the byte offset in s where
// the remainder starts (0 when nothing follows), and ok false when the
// statement does not parse. The vitess parser panics on some input (an
// empty string literal after an executable comment, as in the unit tests);
// that is ok false too.
func doltPiece(s string, ansiQuotes bool) (piece string, next int, ok bool) {
	defer func() {
		if recover() != nil {
			piece, next, ok = "", 0, false
		}
	}()
	lead := len(s) - len(strings.TrimLeftFunc(s, unicode.IsSpace))
	t := trimStatement(s)
	if t == "" {
		return "", 0, true
	}
	opts := sqlparser.ParserOptions{AnsiQuotes: ansiQuotes}
	_, ri, err := sqlparser.ParseOneWithOptions(context.Background(), t, opts)
	switch {
	case err == sqlparser.ErrEmpty:
		// Only comments before the next statement, if any.
	case err != nil:
		return "", 0, false
	case ri > 0 && ri < len(t):
		piece = t[:ri]
	default:
		piece = t
	}
	if ri <= 0 || ri >= len(t) {
		return piece, 0, true
	}
	return piece, lead + ri, true
}

// trimStatement trims what go-mysql-server trims before each parse: spaces,
// and trailing semicolons.
func trimStatement(s string) string {
	return strings.TrimRightFunc(strings.TrimSpace(s), func(r rune) bool {
		return r == ';' || unicode.IsSpace(r)
	})
}

// lexedWrite reports whether query holds a write under any of the lexings
// IsWriteArgs describes.
func lexedWrite(query string, args []driver.NamedValue) bool {
	// Whether a backslash escapes a quote, or a double quote starts a string,
	// depends on the session's sql_mode (NO_BACKSLASH_ESCAPES, ANSI_QUOTES),
	// which the client cannot see; the lexings can draw different statement
	// boundaries, so any one of them finding a write is enough.
	lexings := [][]token{tokenize(query, true), tokenize(query, false)}
	for _, ansi := range []bool{false, true} {
		toks, ok := vitessTokenize(query, ansi)
		if !ok {
			return true
		}
		lexings = append(lexings, toks)
	}
	for _, toks := range lexings {
		for _, stmt := range splitStatements(toks) {
			if !isRead(stmt, args) {
				return true
			}
		}
	}
	return false
}

// vitessTokenize lexes query with the tokenizer Dolt's server and embedded
// driver split and parse with, mapped onto this file's tokens. ok is false
// when vitess cannot lex the query.
func vitessTokenize(query string, ansiQuotes bool) (toks []token, ok bool) {
	tkn := sqlparser.NewStringTokenizer(query)
	if ansiQuotes {
		tkn = sqlparser.NewStringTokenizerForAnsiQuotes(query)
	}
	for {
		typ, val := tkn.Scan()
		switch {
		case typ == 0:
			return toks, tkn.LastError == nil
		case typ == sqlparser.LEX_ERROR:
			return nil, false
		case typ == sqlparser.COMMENT:
		case typ == sqlparser.STRING:
			toks = append(toks, token{tString, string(val)})
		case typ == sqlparser.VALUE_ARG && strings.HasPrefix(string(val), ":v"):
			toks = append(toks, token{tPunct, "?"}) // a ? placeholder, renamed :vN
		case typ < 256:
			toks = append(toks, token{tPunct, string(rune(typ))})
		case typ == sqlparser.ID && mutatingKeywords[strings.ToUpper(string(val))]:
			// vitess returns an unquoted keyword as its keyword token, so an
			// identifier spelled like one was quoted: `delete`.
			toks = append(toks, token{tIdent, string(val)})
		case val != nil:
			// A keyword, identifier (quoted or not), number or variable.
			toks = append(toks, token{tWord, strings.ToUpper(string(val))})
		default:
			// A multi-byte operator: never a statement boundary or keyword.
			toks = append(toks, token{tPunct, fmt.Sprintf("op%d", typ)})
		}
	}
}

type tokKind int

const (
	tWord   tokKind = iota // keyword or bare identifier, upper-cased; @ and @@ kept
	tString                // '...' or "..." literal, unquoted
	tIdent                 // `...` identifier, unquoted
	tPunct                 // one byte of anything else
)

type token struct {
	kind tokKind
	text string
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c == '@' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' || c >= 0x80
}

// tokenize lexes query the way MySQL does for the purpose of finding
// statement boundaries and keywords: quoted text is one token, comments are
// dropped, and an executable comment's body is lexed as SQL. backslash says
// whether a backslash escapes the next byte inside '...' and "...".
func tokenize(q string, backslash bool) []token {
	var toks []token
	inExecComment := false
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v':
			i++
		case c == '/' && strings.HasPrefix(q[i:], "/*!"), c == '/' && strings.HasPrefix(q[i:], "/*+"):
			// Executable comment: its body is SQL. MySQL does not nest them.
			i += 3
			for i < len(q) && q[i] >= '0' && q[i] <= '9' {
				i++ // /*!40000 version prefix
			}
			inExecComment = true
		case c == '*' && inExecComment && strings.HasPrefix(q[i:], "*/"):
			i += 2
			inExecComment = false
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				return toks
			}
			i += 2 + end + 2
		case c == '#', c == '-' && isDashComment(q[i:]):
			end := strings.IndexByte(q[i:], '\n')
			if end < 0 {
				return toks
			}
			i += end + 1
		case c == '\'' || c == '"' || c == '`':
			j, text := scanQuoted(q, i, backslash)
			kind := tString
			if c == '`' {
				kind = tIdent
			}
			toks = append(toks, token{kind, text})
			i = j
		case isWordByte(c):
			j := i
			for j < len(q) && isWordByte(q[j]) {
				j++
			}
			toks = append(toks, token{tWord, strings.ToUpper(q[i:j])})
			i = j
		default:
			toks = append(toks, token{tPunct, q[i : i+1]})
			i++
		}
	}
	return toks
}

// isDashComment reports whether s starts a "-- " comment: MySQL requires the
// second dash to be followed by whitespace, a control character or the end.
func isDashComment(s string) bool {
	if !strings.HasPrefix(s, "--") {
		return false
	}
	return len(s) == 2 || s[2] <= ' '
}

// scanQuoted returns the index just past the quoted run starting at q[i] and
// its unescaped text. A doubled quote escapes itself; with backslash set, a
// backslash (outside backticks) escapes the next byte. An unterminated run
// extends to the end.
func scanQuoted(q string, i int, backslash bool) (int, string) {
	quote := q[i]
	var b strings.Builder
	j := i + 1
	for j < len(q) {
		c := q[j]
		switch {
		case backslash && c == '\\' && quote != '`' && j+1 < len(q):
			b.WriteByte(q[j+1])
			j += 2
		case c == quote && j+1 < len(q) && q[j+1] == quote:
			b.WriteByte(quote)
			j += 2
		case c == quote:
			return j + 1, b.String()
		default:
			b.WriteByte(c)
			j++
		}
	}
	return j, b.String()
}

func splitStatements(toks []token) [][]token {
	var out [][]token
	start := 0
	for i, t := range toks {
		if t.kind == tPunct && t.text == ";" {
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	return append(out, toks[start:])
}

var mutatingKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "INTO": true,
}

func hasWord(stmt []token, words map[string]bool) bool {
	for _, t := range stmt {
		if t.kind == tWord && words[t.text] {
			return true
		}
	}
	return false
}

// mutates reports whether a query statement carries a mutating keyword. The
// locking read's FOR UPDATE is not one.
func mutates(stmt []token) bool {
	for i, t := range stmt {
		if t.kind != tWord || !mutatingKeywords[t.text] {
			continue
		}
		if t.text == "UPDATE" && i > 0 && stmt[i-1].kind == tWord && stmt[i-1].text == "FOR" {
			continue
		}
		return true
	}
	return false
}

func isPunct(t token, p string) bool { return t.kind == tPunct && t.text == p }

func isRead(stmt []token, args []driver.NamedValue) bool {
	// Leading parentheses group a query expression: (SELECT ...) UNION ...
	for len(stmt) > 0 && isPunct(stmt[0], "(") {
		stmt = stmt[1:]
	}
	if len(stmt) == 0 {
		return true
	}
	if stmt[0].kind != tWord {
		return false
	}
	switch stmt[0].text {
	case "SELECT", "TABLE", "VALUES", "WITH", "SHOW":
		return !mutates(stmt)
	case "EXPLAIN", "DESCRIBE", "DESC":
		// Plain EXPLAIN only plans its statement; EXPLAIN ANALYZE runs it.
		return !hasWord(stmt, map[string]bool{"ANALYZE": true}) || !mutates(stmt)
	case "USE", "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT":
		return true
	case "RELEASE":
		return len(stmt) > 1 && stmt[1].kind == tWord && stmt[1].text == "SAVEPOINT"
	case "START":
		return len(stmt) > 1 && stmt[1].kind == tWord && stmt[1].text == "TRANSACTION"
	case "SET":
		return isSessionSet(stmt[1:])
	case "CREATE":
		return isEnsureDatabase(stmt[1:])
	case "CALL":
		return isBranchSwitch(stmt[1:], args)
	}
	return false
}

var globalSetWords = map[string]bool{
	"GLOBAL": true, "PERSIST": true, "PERSIST_ONLY": true,
	"@@GLOBAL": true, "@@PERSIST": true, "@@PERSIST_ONLY": true,
}

// isSessionSet reports whether the tokens after SET change only this
// session's state.
func isSessionSet(rest []token) bool {
	if len(rest) == 0 || rest[0].kind != tWord {
		return false
	}
	switch rest[0].text {
	case "PASSWORD", "DEFAULT", "RESOURCE":
		return false
	}
	return !hasWord(rest, globalSetWords)
}

// isEnsureDatabase reports whether rest (what follows CREATE) is exactly
// "DATABASE|SCHEMA IF NOT EXISTS <name>".
func isEnsureDatabase(rest []token) bool {
	if len(rest) != 5 {
		return false
	}
	for i, want := range []string{"", "IF", "NOT", "EXISTS"} {
		if rest[i].kind != tWord {
			return false
		}
		if i == 0 && rest[0].text != "DATABASE" && rest[0].text != "SCHEMA" {
			return false
		}
		if i > 0 && rest[i].text != want {
			return false
		}
	}
	return rest[4].kind == tWord || rest[4].kind == tIdent
}

// bdBranches are the branch names bd passes to DOLT_CHECKOUT as literals.
var bdBranches = map[string]bool{"main": true, "flatten-tmp": true, "compact-tmp": true}

// isBranchSwitch reports whether rest (what follows CALL) is
// DOLT_CHECKOUT(<one of bdBranches>) or DOLT_CHECKOUT(<? bound to a non-flag>). A ? placeholder is resolved from
// args; unresolvable, it counts as a write.
func isBranchSwitch(rest []token, args []driver.NamedValue) bool {
	if len(rest) != 4 || rest[0].kind != tWord || rest[0].text != "DOLT_CHECKOUT" ||
		!isPunct(rest[1], "(") || !isPunct(rest[3], ")") {
		return false
	}
	arg := rest[2]
	switch {
	case arg.kind == tString:
		return bdBranches[arg.text]
	case isPunct(arg, "?"):
		if len(args) != 1 {
			return false
		}
		s, ok := args[0].Value.(string)
		return ok && s != "" && !strings.HasPrefix(s, "-")
	}
	return false
}
