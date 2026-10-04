//go:build integration && !windows

package dolt

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/memoryops"
)

// TestMigration0070AddsMemoryJSONToExistingStoreAsRawSQL pins migration 0070
// on the path existing stores actually take. Every clone that journaled before
// 0070 existed has a bd_events_journal without memory_json, and it gains the
// column only through 0070's INFORMATION_SCHEMA-guarded PREPARE block, run as
// raw SQL -- never through the CLI-bundle override, which is all
// TestMigration0070MemoryJSONLandsThroughDoltCLI in internal/storage/schema
// exercises. Without this test, deleting the guarded block from the .up.sql
// fails only the byte pins in that package.
//
// setupTestStore migrates to latest, so the raw down (itself under test here)
// is what removes the column. Then the raw up runs twice through
// runMigrationSQL: pass 1 must fire the guard, and pass 2 must be a clean
// no-op on a store already carrying the column. Afterwards a `bd remember`
// through the store must journal, which is the whole reason the column exists
// -- and the failure the ignored twin guards against on the fresh-clone door
// (a journal INSERT naming an unknown column rolls the user's write back).
func TestMigration0070AddsMemoryJSONToExistingStoreAsRawSQL(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	const upFile = "../schema/migrations/0070_add_events_journal_memory.up.sql"
	const downFile = "../schema/migrations/0070_add_events_journal_memory.down.sql"

	requireColumnPresence(ctx, t, store, "bd_events_journal", "memory_json", true)

	runMigrationSQL(t, ctx, store, downFile)
	requireColumnPresence(ctx, t, store, "bd_events_journal", "memory_json", false)

	for pass := 1; pass <= 2; pass++ {
		runMigrationSQL(t, ctx, store, upFile)
		requireColumnPresence(ctx, t, store, "bd_events_journal", "memory_json", true)
		requireColumnType(ctx, t, store, "bd_events_journal", "memory_json", "longtext")
	}

	enableJournalForTest(t, store)
	clearJournal(t, store)
	memories, err := store.Memories()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memories.Remember(ctx, memoryops.RememberRequest{Key: "mig0070", Content: "journaled after the raw up", Actor: "actor"}); err != nil {
		t.Fatalf("remember after the raw 0070 up: %v", err)
	}
	var memoryJSON string
	if err := store.db.QueryRowContext(ctx,
		`SELECT memory_json FROM bd_events_journal WHERE op = 'memory_remember' ORDER BY seq DESC LIMIT 1`).Scan(&memoryJSON); err != nil {
		t.Fatalf("read the memory_remember row: %v", err)
	}
	if memoryJSON == "" {
		t.Fatal("memory_remember row has an empty memory_json after the raw 0070 up")
	}
}

// requireColumnPresence fails unless INFORMATION_SCHEMA agrees with want about
// whether table.column exists.
func requireColumnPresence(ctx context.Context, t *testing.T, store *DoltStore, table, column string, want bool) {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&count); err != nil {
		t.Fatalf("probe %s.%s: %v", table, column, err)
	}
	if got := count > 0; got != want {
		t.Fatalf("%s.%s present = %v, want %v", table, column, got, want)
	}
}
