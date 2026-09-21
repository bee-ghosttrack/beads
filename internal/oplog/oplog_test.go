package oplog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func fixedNow() time.Time { return time.Date(2026, 9, 21, 23, 59, 0, 0, time.UTC) }

func TestBeginEnd_WritesIntentThenOutcome(t *testing.T) {
	dir := t.TempDir()
	op, err := Begin(Options{Dir: dir, Now: fixedNow, Host: "studio"}, Intent{
		Actor: "bee",
		Verb:  "update",
		Args:  []string{"bd-1"},
		IDs:   []string{"bd-1"},
		Flags: map[string]string{"status": "closed", "notes": "secret body text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.End(3); err != nil {
		t.Fatal(err)
	}
	if err := op.End(0); err != nil { // idempotent: second call writes nothing
		t.Fatal(err)
	}

	path := filepath.Join(dir, "studio-20260921.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret body text") {
		t.Fatal("flag value leaked into the op log")
	}
	recs := readRecords(t, path)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	in, out := recs[0], recs[1]
	if in.Phase != PhaseIntent || out.Phase != PhaseOutcome {
		t.Fatalf("phases %q,%q", in.Phase, out.Phase)
	}
	if in.OpID != op.ID() || out.OpID != op.ID() || len(op.ID()) != 26 {
		t.Fatalf("op ids %q %q %q", in.OpID, out.OpID, op.ID())
	}
	if in.Actor != "bee" || in.Verb != "update" || in.Host != "studio" || in.PID != os.Getpid() {
		t.Fatalf("intent fields: %+v", in)
	}
	if strings.Join(in.Flags, ",") != "notes,status" {
		t.Fatalf("flags %v", in.Flags)
	}
	wantSum, wantLen := PayloadDigest("update", []string{"bd-1"}, map[string]string{"status": "closed", "notes": "secret body text"})
	if in.PayloadSHA256 != wantSum || in.PayloadBytes != wantLen {
		t.Fatalf("digest %s/%d, want %s/%d", in.PayloadSHA256, in.PayloadBytes, wantSum, wantLen)
	}
	if out.RC == nil || *out.RC != 3 {
		t.Fatalf("outcome rc %v", out.RC)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestPayloadDigest_ValueSensitiveOrderInsensitive(t *testing.T) {
	a, _ := PayloadDigest("create", []string{"x"}, map[string]string{"a": "1", "b": "2"})
	b, _ := PayloadDigest("create", []string{"x"}, map[string]string{"b": "2", "a": "1"})
	c, _ := PayloadDigest("create", []string{"x"}, map[string]string{"a": "1", "b": "3"})
	if a != b {
		t.Fatal("digest depends on map order")
	}
	if a == c {
		t.Fatal("digest ignores flag values")
	}
}

func TestBegin_CapsIDs(t *testing.T) {
	dir := t.TempDir()
	ids := make([]string, maxIDs+5)
	for i := range ids {
		ids[i] = "bd-x"
	}
	op, err := Begin(Options{Dir: dir, Now: fixedNow, Host: "h"}, Intent{Verb: "close", IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, filepath.Join(dir, "h-20260921.jsonl"))
	if len(recs[0].IDs) != maxIDs || !recs[0].IDsTruncated {
		t.Fatalf("ids %d truncated=%v", len(recs[0].IDs), recs[0].IDsTruncated)
	}
	_ = op.End(0)
}

func TestNilOpEnd(t *testing.T) {
	var op *Op
	if err := op.End(1); err != nil {
		t.Fatal(err)
	}
}

func TestULID_SortsByTimeAndIsWellFormed(t *testing.T) {
	a, _ := NewULID(time.UnixMilli(1_000))
	b, _ := NewULID(time.UnixMilli(2_000))
	if len(a) != 26 || a >= b {
		t.Fatalf("a=%s b=%s", a, b)
	}
	if !strings.HasPrefix(a, "000000") {
		t.Fatalf("time prefix of a small timestamp should be zeros: %s", a)
	}
	for _, r := range a {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("non-crockford rune %q in %s", r, a)
		}
	}
	// 2026 timestamps start with 01 (ULIDs overflow '7' in 10889 AD).
	c, _ := NewULID(fixedNow())
	if c[0] != '0' || c[1] != '1' {
		t.Fatalf("2026 ULID %s", c)
	}
}

func TestConcurrentAppendsStayWholeLines(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := Begin(Options{Dir: dir, Now: fixedNow, Host: "h"}, Intent{Verb: "update", IDs: []string{"bd-1"}})
			if err != nil {
				t.Error(err)
				return
			}
			_ = op.End(0)
		}()
	}
	wg.Wait()
	recs := readRecords(t, filepath.Join(dir, "h-20260921.jsonl"))
	if len(recs) != 64 {
		t.Fatalf("want 64 records, got %d", len(recs))
	}
}

func TestBegin_EmptyDir(t *testing.T) {
	if _, err := Begin(Options{}, Intent{Verb: "create"}); err == nil {
		t.Fatal("want error for empty dir")
	}
}

func BenchmarkBeginEnd(b *testing.B) {
	for _, s := range []bool{false, true} {
		name := "pagecache"
		if s {
			name = "fsync"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			in := Intent{Actor: "bee", Verb: "update", Args: []string{"bd-1"}, IDs: []string{"bd-1"}, Flags: map[string]string{"status": "in_progress"}}
			for i := 0; i < b.N; i++ {
				op, err := Begin(Options{Dir: dir, Sync: s, Host: "h"}, in)
				if err != nil {
					b.Fatal(err)
				}
				_ = op.End(0)
			}
		})
	}
}
