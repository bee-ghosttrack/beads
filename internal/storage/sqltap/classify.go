package sqltap

import (
	"database/sql/driver"
	"strings"
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
//   - SELECT, TABLE, VALUES, WITH, SHOW or HELP with no INTO clause and no
//     INSERT/UPDATE/DELETE/REPLACE keyword (FOR UPDATE aside);
//   - EXPLAIN, DESCRIBE, DESC — but EXPLAIN ANALYZE runs its statement, so
//     it is judged by the same keywords;
//   - USE, and a session-scoped SET (not GLOBAL, PERSIST, PERSIST_ONLY,
//     PASSWORD, DEFAULT ROLE or RESOURCE GROUP);
//   - transaction control: BEGIN, START TRANSACTION, COMMIT, ROLLBACK,
//     SAVEPOINT, RELEASE SAVEPOINT;
//   - CREATE DATABASE|SCHEMA IF NOT EXISTS <name>, which every store open
//     sends and which creates nothing once the database exists;
//   - CALL DOLT_CHECKOUT with exactly one argument that is not a flag: a
//     session branch switch. Dolt gives a table name the same syntax (it
//     resets that table's working set), which this cannot tell apart; bd
//     only ever passes a branch.
//
// Anything else, including any statement this cannot parse, is a write. The
// comment forms MySQL executes — /*! ... */ and /*+ ... */ — are read as SQL,
// not skipped. The query is lexed both with and without backslash escapes in
// quoted text, and a write under either counts.
func IsWriteArgs(query string, args []driver.NamedValue) bool {
	// Whether a backslash escapes a quote depends on the session's sql_mode
	// (NO_BACKSLASH_ESCAPES), which the client cannot see; the two lexings can
	// draw different statement boundaries, so either one finding a write is
	// enough.
	for _, backslash := range []bool{true, false} {
		for _, stmt := range splitStatements(tokenize(query, backslash)) {
			if !isRead(stmt, args) {
				return true
			}
		}
	}
	return false
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
	case "SELECT", "TABLE", "VALUES", "WITH", "SHOW", "HELP":
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

// isBranchSwitch reports whether rest (what follows CALL) is
// DOLT_CHECKOUT(<one non-flag argument>). A ? placeholder is resolved from
// args; unresolvable, it counts as a write.
func isBranchSwitch(rest []token, args []driver.NamedValue) bool {
	if len(rest) != 4 || rest[0].kind != tWord || rest[0].text != "DOLT_CHECKOUT" ||
		!isPunct(rest[1], "(") || !isPunct(rest[3], ")") {
		return false
	}
	arg := rest[2]
	switch {
	case arg.kind == tString:
		return arg.text != "" && !strings.HasPrefix(arg.text, "-")
	case isPunct(arg, "?"):
		if len(args) != 1 {
			return false
		}
		s, ok := args[0].Value.(string)
		return ok && s != "" && !strings.HasPrefix(s, "-")
	}
	return false
}
