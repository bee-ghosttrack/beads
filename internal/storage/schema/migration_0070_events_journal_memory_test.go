package schema

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// Migration 0070 (gastownhall/beads: journal the memory plane): the durable
// events journal recorded every bead mutation and no memory write, because a
// memory is a config-table row under kv.memory. and the config seam journals
// nothing. issueops.RecordMemoryEventInTx now records memory_remember and
// memory_forget rows, whose payload — the user key, the content after a
// remember, the value before a replace or a forget — needs a column of its
// own: memory_json, nullable like the three payload columns beside it. The
// shape is 0066's (a guarded prepared ADD COLUMN on the always-present journal
// table), so it carries 0066's three obligations: a CLI-bundle direct-DDL
// override (dolthub/dolt#11345), an ignored-series twin (ignored/0028, check D
// of scripts/check-migration-hygiene.sh), and this pin.
const migration0070Up = "0070_add_events_journal_memory.up.sql"
const migration0070Down = "0070_add_events_journal_memory.down.sql"

const migration0070MemoryGuard = "@needs_memory_json"

// TestLatestVersionIncludesMigration0070 pins the real next free slot this
// migration claims, superseding 0069's own version of this test
// (LatestVersion() moved from 69 to 70 the moment this migration file was
// added). Deliberately a hardcoded literal for the same reason 0067's, 0068's
// and 0069's were: LatestVersion() drifting to 70 for the wrong reason (an
// unrelated migration landing first) should still be caught by this test
// failing to explain why 70 is memory_json-shaped, which the pure-Go test
// below checks.
func TestLatestVersionIncludesMigration0070(t *testing.T) {
	const want = 70
	if got := LatestVersion(); got != want {
		t.Fatalf("LatestVersion() = %d, want %d (bd_events_journal.memory_json migration slot claimed by the memory-plane journaling change)", got, want)
	}
}

// TestMigration0070AddsEventsJournalMemoryJSON is the pure-Go, DB-independent
// half of the pin, mirroring TestMigration0069WidensChangeAtAndRemovedAtPrecision:
// the frozen migration bytes, the CLI-bundle override text and the ignored twin
// are checked directly, no `dolt` binary required.
func TestMigration0070AddsEventsJournalMemoryJSON(t *testing.T) {
	upSQL, err := MigrationSQL(migration0070Up)
	if err != nil {
		t.Fatalf("MigrationSQL(%s) error = %v, want the migration file to exist", migration0070Up, err)
	}
	for _, want := range []string{
		"ALTER TABLE bd_events_journal ADD COLUMN memory_json LONGTEXT",
		migration0070MemoryGuard,
		"COLUMN_NAME = 'memory_json'",
		"TABLE_NAME = 'bd_events_journal'",
	} {
		if !strings.Contains(upSQL, want) {
			t.Errorf("0070 up migration missing %q (bd_events_journal.memory_json)\nfull SQL:\n%s", want, upSQL)
		}
	}
	if !strings.Contains(strings.ToUpper(upSQL), "PREPARE STMT FROM @SQL") {
		t.Error("0070 up migration must keep its guarded PREPARE block -- it is what makes a raw .up.sql replay onto a store already carrying the column a no-op")
	}

	bundle := cliCompatibleMigrationSQL(migration0070Up, upSQL)
	if want := "ALTER TABLE bd_events_journal ADD COLUMN memory_json LONGTEXT;"; !strings.Contains(bundle, want) {
		t.Errorf("0070's CLI bundle substitute (cliMigration0070AddEventsJournalMemory) missing direct DDL %q -- without it a fresh CLI-built database has no memory_json while the runtime migration path adds it, and every `bd remember` on such a workspace fails its journal INSERT and rolls back", want)
	}
	if strings.Contains(bundle, migration0070MemoryGuard) {
		t.Errorf("0070's CLI bundle substitute still carries the prepared guard %q, which a pre-2.3 Dolt CLI silently no-ops", migration0070MemoryGuard)
	}
	if cliSubstituteAssumesWispTables(migration0070Up) {
		t.Error("0070's CLI substitute touches only bd_events_journal, which 0064 always creates — it must not be listed in cliSubstituteAssumesWispTables")
	}

	// The ignored-series twin carries the same column through the fresh-clone
	// door (ignored/0025 is the precedent, for actor). Its guard and DDL are
	// the up migration's, byte for byte, so the two doors converge.
	twin, err := IgnoredMigrationSQL("0028_add_events_journal_memory.up.sql")
	if err != nil {
		t.Fatalf("ignored twin of 0070: %v, want ignored/0028 to exist (check D of scripts/check-migration-hygiene.sh)", err)
	}
	for _, want := range []string{
		"ALTER TABLE bd_events_journal ADD COLUMN memory_json LONGTEXT",
		migration0070MemoryGuard,
		"TABLE_NAME = 'bd_events_journal'",
	} {
		if !strings.Contains(twin, want) {
			t.Errorf("ignored/0028 missing %q; a fresh clone would materialize the journal without memory_json", want)
		}
	}

	// down.sql files are not part of the embedded FS (only migrations/*.up.sql
	// is //go:embed'd -- see mainSource.files), so this reads straight from
	// disk by package-relative path, as 0069's down check does.
	downBytes, err := os.ReadFile("migrations/" + migration0070Down)
	if err != nil {
		t.Fatalf("read %s: %v, want the migration file to exist", migration0070Down, err)
	}
	downSQL := string(downBytes)
	for _, want := range []string{
		"ALTER TABLE bd_events_journal DROP COLUMN memory_json",
		"COLUMN_NAME = 'memory_json'",
	} {
		if !strings.Contains(downSQL, want) {
			t.Errorf("0070 down migration missing %q\nfull SQL:\n%s", want, downSQL)
		}
	}
	if !strings.Contains(strings.ToUpper(downSQL), "PREPARE STMT FROM @SQL") {
		t.Error("0070 down migration must guard its DROP the way the up migration guards its ADD, so a store that never took the column, or was already rolled back, rolls back safely")
	}
}

// TestMigration0070MemoryJSONLandsThroughDoltCLI is the live half: the CLI
// fresh bundle, run through a real `dolt` binary (skipped without one), leaves
// bd_events_journal with a nullable longtext memory_json — the shape the
// runtime migration path produces, which is what the override exists to match.
func TestMigration0070MemoryJSONLandsThroughDoltCLI(t *testing.T) {
	testutil.RequireDoltBinary(t)

	dir := t.TempDir()
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")
	runDoltSQL(t, dir, AllMigrationsSQL())

	requireDoltColumnShape(t, dir, "bd_events_journal", "memory_json", "longtext", "YES")
}
