package issueops

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestEventOpVocabularyIsFrozen pins the journal's op vocabulary and its split
// into the six-op public wire set plus the three-op engine-only remainder (the
// comment write and the two memory-plane writes).
//
// The two sets are declared independently of the EventOp constants, so this
// fails on any drift in either direction: a new EventOp constant that nobody
// assigned to a side, a wire op quietly renamed, or an engine-only op promoted
// to the wire without the contract change that would require. Consumers dedupe
// and switch on these strings, so a rename is a breaking change even when it
// compiles.
func TestEventOpVocabularyIsFrozen(t *testing.T) {
	const (
		wantWire       = 6
		wantEngineOnly = 3
	)

	frozenWire := []EventOp{"create", "update", "close", "delete", "dep_add", "dep_remove"}
	frozenEngineOnly := []EventOp{"comment", "memory_remember", "memory_forget"}

	if got := WireEventOps(); !equalOps(got, frozenWire) {
		t.Errorf("wire event vocabulary changed: got %v, frozen %v", got, frozenWire)
	}
	if got := EngineOnlyEventOps(); !equalOps(got, frozenEngineOnly) {
		t.Errorf("engine-only event vocabulary changed: got %v, frozen %v", got, frozenEngineOnly)
	}
	if len(frozenWire) != wantWire || len(frozenEngineOnly) != wantEngineOnly {
		t.Fatalf("vocabulary sizes changed: %d wire, %d engine-only", len(frozenWire), len(frozenEngineOnly))
	}

	for _, op := range frozenWire {
		if !IsWireEventOp(op) {
			t.Errorf("IsWireEventOp(%q) = false, want true", op)
		}
	}
	for _, op := range frozenEngineOnly {
		if IsWireEventOp(op) {
			t.Errorf("IsWireEventOp(%q) = true, want false: engine-only ops must not reach the wire", op)
		}
	}
	if IsWireEventOp("not_an_op") {
		t.Error("IsWireEventOp accepted an unknown op")
	}
}

// TestEveryDeclaredEventOpIsClassified is the structural half: it reads the
// EventOp constants out of journal.go's AST and asserts every one of them lands
// on exactly one side of the freeze. Declaring a new op without classifying it
// therefore cannot ship — which is the failure mode a hand-maintained list has.
func TestEveryDeclaredEventOpIsClassified(t *testing.T) {
	declared := declaredEventOps(t)
	if len(declared) == 0 {
		t.Fatal("found no EventOp constants in journal.go — was the type renamed?")
	}

	classified := map[EventOp]int{}
	for _, op := range WireEventOps() {
		classified[op]++
	}
	for _, op := range EngineOnlyEventOps() {
		classified[op]++
	}

	for _, op := range declared {
		switch classified[op] {
		case 1:
			// classified exactly once
		case 0:
			t.Errorf("EventOp %q is journaled but classified on neither side: add it to wireEventOps or engineOnlyEventOps", op)
		default:
			t.Errorf("EventOp %q is classified as both wire and engine-only", op)
		}
	}
	if len(declared) != len(classified) {
		t.Errorf("classified %d ops but %d are declared: %v", len(classified), len(declared), declared)
	}
}

// declaredEventOps returns the string value of every `X EventOp = "y"` constant
// declared in journal.go.
func declaredEventOps(t *testing.T) []EventOp {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "journal.go", nil, 0)
	if err != nil {
		t.Fatalf("parse journal.go: %v", err)
	}

	var ops []EventOp
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "EventOp" {
				continue
			}
			for _, val := range vs.Values {
				lit, ok := val.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				ops = append(ops, EventOp(lit.Value[1:len(lit.Value)-1]))
			}
		}
	}
	return ops
}

func equalOps(a, b []EventOp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRecordMemoryEventInTxRefusesMalformedCalls pins the memory emit's two
// refusals — a non-memory op, and a payload with no key — and that a disabled
// journal makes it a no-op before any SQL. The refusals matter because the
// alternative is a row consumers cannot decode: a memory payload under an op
// they parse as something else, or a memory record naming no memory.
func TestRecordMemoryEventInTxRefusesMalformedCalls(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	on := WithEventsJournal(context.Background(), true)
	memory := &EventMemory{Key: "k"}

	if err := RecordMemoryEventInTx(on, tx, EventCreate, memory, "actor"); err == nil {
		t.Error("a non-memory op was accepted; the payload would land under an op consumers decode as a bead mutation")
	}
	if err := RecordMemoryEventInTx(on, tx, EventMemoryRemember, nil, "actor"); err == nil {
		t.Error("a nil payload was accepted; the record would name no memory")
	}
	if err := RecordMemoryEventInTx(on, tx, EventMemoryForget, &EventMemory{}, "actor"); err == nil {
		t.Error("an empty key was accepted; the record would name no memory")
	}
	off := WithEventsJournal(context.Background(), false)
	if err := RecordMemoryEventInTx(off, tx, EventMemoryRemember, memory, "actor"); err != nil {
		t.Errorf("disabled journal: %v, want a silent no-op", err)
	}
	// None of the above may have reached the database.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}
