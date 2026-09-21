// Package oplog is bd's client-side write-ahead operation log.
//
// When oplog.dir is set, every command that writes appends two JSONL records
// to a per-host, per-UTC-day file in that directory: an "intent" record before
// its first write leaves the process, and an "outcome" record after it
// finishes. The pair lets an operator prove, from the client side alone,
// which writes a seat attempted, which ones it believes succeeded, and which
// ones it never heard back about — independently of whatever the server kept.
//
// Bodies are never logged. The intent carries a SHA-256 digest and a byte
// count of the command's canonical payload (verb, positional args, changed
// flag values), which is enough to prove what was sent without copying issue
// text into a second place. Each record is one write(2) on an O_APPEND file,
// so concurrent bd processes on the same host interleave whole lines.
//
// Durability defaults to the page cache: a killed process loses nothing it
// already wrote. oplog.sync=true adds an fsync per record, for a seat whose
// server lives on another machine.
//
// Limits, stated so nobody reads more into the log than it holds:
//   - The intent is written by the caller at the command's first mutating
//     SQL statement (bd arms it through internal/storage/sqltap), so a
//     preview or a command that fails before writing logs nothing, and a
//     read command whose store open migrates the schema is logged as the
//     write it is. Writes bd makes without a client SQL connection (the
//     dbproxy server's own, and anything done by another process) are not
//     logged.
//   - Records carry no issue IDs: the arguments a command was given are not
//     a list of what it touched.
//   - A panic, or a kill -9, leaves an intent with no outcome. That is the
//     signal the log exists to give, but it is not proof the write was lost.
//   - The digest is not a secret. For a low-entropy payload (a status or
//     priority value) it can be reversed by guessing, and payload_bytes
//     reveals the payload's length.
//   - One append per record is atomic on local filesystems. Put oplog.dir on
//     local disk, not NFS.
package oplog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase values for Record.Phase.
const (
	PhaseIntent  = "intent"
	PhaseOutcome = "outcome"
)

// maxIDs bounds how many issue IDs one record carries, so a bulk command
// cannot grow a record past the size a single append stays atomic at.
const maxIDs = 64

// Record is one line of the op log. Intent and outcome records share the
// type; fields that do not apply to a phase are omitted.
type Record struct {
	OpID          string   `json:"op_id"`
	Phase         string   `json:"phase"`
	TS            string   `json:"ts"`
	Host          string   `json:"host,omitempty"`
	PID           int      `json:"pid,omitempty"`
	Actor         string   `json:"actor,omitempty"`
	Verb          string   `json:"verb,omitempty"`
	IDs           []string `json:"ids,omitempty"`
	IDsTruncated  bool     `json:"ids_truncated,omitempty"`
	Flags         []string `json:"flags,omitempty"`
	PayloadSHA256 string   `json:"payload_sha256,omitempty"`
	PayloadBytes  int      `json:"payload_bytes,omitempty"`
	RC            *int     `json:"rc,omitempty"`
	ElapsedMS     *int64   `json:"elapsed_ms,omitempty"`
}

// Intent describes a mutating command about to run.
type Intent struct {
	Actor string
	// Verb is the command path below the root, e.g. "create" or "dep add".
	Verb string
	// Args are the positional arguments; they feed the payload digest.
	Args []string
	// IDs are the issue IDs the command names, when known up front.
	IDs []string
	// Flags maps each flag the caller set to its value. Names are logged;
	// values only feed the digest.
	Flags map[string]string
}

// Op is an in-flight operation: its intent is on disk, its outcome is not.
type Op struct {
	mu      sync.Mutex
	path    string
	sync    bool
	id      string
	started time.Time
	done    bool
}

// Options configure Begin.
type Options struct {
	Dir  string
	Sync bool
	// Now and Host are test seams; zero values mean time.Now and os.Hostname.
	Now  func() time.Time
	Host string
}

// Begin appends the intent record and returns the Op whose End writes the
// outcome. An error means nothing was logged; callers decide whether that is
// fatal (bd treats it as a warning so the op log can never block a write).
func Begin(opts Options, in Intent) (*Op, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("oplog: empty dir")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	host := opts.Host
	if host == "" {
		host, _ = os.Hostname()
	}
	t := now().UTC()
	id, err := NewULID(t)
	if err != nil {
		return nil, err
	}
	sum, size := PayloadDigest(in.Verb, in.Args, in.Flags)
	ids, truncated := capIDs(in.IDs)
	rec := Record{
		OpID:          id,
		Phase:         PhaseIntent,
		TS:            t.Format(time.RFC3339Nano),
		Host:          host,
		PID:           os.Getpid(),
		Actor:         in.Actor,
		Verb:          in.Verb,
		IDs:           ids,
		IDsTruncated:  truncated,
		Flags:         flagNames(in.Flags),
		PayloadSHA256: sum,
		PayloadBytes:  size,
	}
	path := filepath.Join(opts.Dir, fileName(host, t))
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("oplog: %w", err)
	}
	if err := appendRecord(path, opts.Sync, rec); err != nil {
		return nil, err
	}
	return &Op{path: path, sync: opts.Sync, id: id, started: t}, nil
}

// ID returns the operation's op_id.
func (o *Op) ID() string { return o.id }

// End appends the outcome record. It is idempotent: only the first call
// writes, so every exit path may call it without double-logging.
func (o *Op) End(rc int) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return nil
	}
	o.done = true
	t := time.Now().UTC()
	elapsed := t.Sub(o.started).Milliseconds()
	return appendRecord(o.path, o.sync, Record{
		OpID:      o.id,
		Phase:     PhaseOutcome,
		TS:        t.Format(time.RFC3339Nano),
		RC:        &rc,
		ElapsedMS: &elapsed,
	})
}

// PayloadDigest returns the SHA-256 (hex) and byte length of the canonical
// payload: the verb, the positional args, and the set flags sorted by name,
// as one JSON array. It is exported so a reconciler can recompute it.
func PayloadDigest(verb string, args []string, flags map[string]string) (string, int) {
	names := make([]string, 0, len(flags))
	for k := range flags {
		names = append(names, k)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(names))
	for _, k := range names {
		pairs = append(pairs, [2]string{k, flags[k]})
	}
	if args == nil {
		args = []string{}
	}
	b, _ := json.Marshal([]any{verb, args, pairs})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), len(b)
}

func appendRecord(path string, doSync bool, rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("oplog: %w", err)
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("oplog: %w", err)
	}
	// One Write call per record: O_APPEND makes it land whole at the end of
	// the file even with other bd processes appending concurrently.
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("oplog: %w", err)
	}
	if doSync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("oplog: %w", err)
		}
	}
	return f.Close()
}

func fileName(host string, t time.Time) string {
	h := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, host)
	if h == "" {
		h = "unknown"
	}
	return fmt.Sprintf("%s-%s.jsonl", h, t.Format("20060102"))
}

func flagNames(flags map[string]string) []string {
	if len(flags) == 0 {
		return nil
	}
	names := make([]string, 0, len(flags))
	for k := range flags {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func capIDs(ids []string) ([]string, bool) {
	if len(ids) <= maxIDs {
		return ids, false
	}
	return ids[:maxIDs], true
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID mints a ULID: 48 bits of Unix milliseconds then 80 random bits,
// Crockford base32, 26 characters. Lexical order follows mint time.
func NewULID(t time.Time) (string, error) {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("oplog: %w", err)
	}
	// 128 bits → 26 base32 digits; the first digit carries the top 3 bits.
	out := make([]byte, 26)
	var acc uint64
	var bits uint
	i := 0
	// Leading 2 padding bits so 130 bits divide evenly into 26×5.
	acc, bits = 0, 2
	for _, v := range b {
		acc = acc<<8 | uint64(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[i] = crockford[(acc>>bits)&31]
			i++
		}
	}
	return string(out), nil
}
