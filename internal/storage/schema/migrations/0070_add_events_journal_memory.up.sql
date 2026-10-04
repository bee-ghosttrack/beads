-- Migration 0070: record MEMORY-PLANE writes in the events journal
-- (gastownhall/beads: journal `bd remember` / `bd forget`).
--
-- bd_events_journal records every bead mutation, but a memory is not a bead:
-- it is a config-table row under kv.memory., written through the config seam,
-- which journals nothing. A workspace's memories therefore had no second copy
-- anywhere a journal consumer could replay from. The writer
-- (issueops.RecordMemoryEventInTx) now records memory_remember and
-- memory_forget rows with an empty issue_id and this new payload column,
-- memory_json: {"key","content","previous"} — the user key verbatim, the value
-- after a remember, the value before a replace or a forget. Nullable, like
-- issue_json, dep_json and comment_json beside it: every other op leaves it
-- NULL, and a row written before this column existed reads NULL uniformly.
--
-- Guarded on an INFORMATION_SCHEMA.COLUMNS probe, so it is a no-op on a
-- workspace already carrying the column and idempotent on replay. The
-- table-exists half keeps the file replayable from any intermediate state
-- (the 0066 pattern): a COLUMNS count of 0 reads the same for "no column" and
-- "no table", and ALTERing a missing table would abort the pass.
--
-- IT SHIPS WITH AN IGNORED-SERIES TWIN (ignored/0028), which is not optional:
-- bd_events_journal is a clone-local dolt-ignored table, so a fresh clone
-- materializes it from the ignored series while the main cursor arrives
-- at-latest and never runs this file. That is check D of
-- scripts/check-migration-hygiene.sh — the mechanism 0066/ignored/0025 record.
--
-- The CLI fresh bundle needs the direct-DDL override
-- cliMigration0070AddEventsJournalMemory (cli_migrations.go): a pre-2.3 Dolt
-- CLI no-ops a PREPARE'd ALTER TABLE (dolthub/dolt#11345), the same hazard
-- 0060/0065/0066/0067/0068/0069 each carry an override for.
SET @needs_memory_json = IF(
    (SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'bd_events_journal') > 0
    AND
    (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME = 'bd_events_journal'
          AND COLUMN_NAME = 'memory_json') = 0,
    1, 0
);
SET @sql = IF(@needs_memory_json = 1,
    'ALTER TABLE bd_events_journal ADD COLUMN memory_json LONGTEXT',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
