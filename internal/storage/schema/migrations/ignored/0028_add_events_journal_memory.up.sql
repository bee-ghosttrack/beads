-- Ignored migration 0028: ensure bd_events_journal carries the memory_json
-- column on every clone.
--
-- The twin of synced migration 0070, and required by check D of
-- scripts/check-migration-hygiene.sh rather than offered: bd_events_journal is
-- clone-local and dolt-ignored, so a fresh clone materializes it from
-- ignored/0022 — whose CREATE carries no memory_json column — while the main
-- cursor arrives at-latest and 0070 never runs there. Without this file an
-- upgraded workspace would journal memory rows while a fresh clone's journal
-- INSERT fails on the unknown column — and because the journal row commits in
-- the mutation's own transaction, that failure would roll back the user's
-- `bd remember`. Same fresh-clone door ignored/0025 (actor), ignored/0024,
-- ignored/0021, ignored/0020 and ignored/0013 each record.
--
-- The definition mirrors 0070 exactly: memory_json is the nullable payload of
-- a memory_remember / memory_forget record, NULL on every other op and on rows
-- written before the column existed. Guarded on the same INFORMATION_SCHEMA
-- probe (with the explicit table-exists half, since a COLUMNS count of 0 cannot
-- tell "no column" from "no table"), so it is a no-op on the fresh-init door and
-- on an already-altered clone, and idempotent on replay. Shape convergence
-- across the two doors is pinned by
-- internal/storage/embeddeddolt/migrate_ignored_plane_shape_test.go.
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
