-- Reverse of 0070: drop bd_events_journal.memory_json. Guarded on the same
-- INFORMATION_SCHEMA probe so a store that never took the column, or was
-- already rolled back, no-ops. Memory rows already journaled keep their op and
-- lose their payload; a consumer replaying across the rollback must
-- re-baseline, as it must across any schema rollback.
--
-- Only migrations/*.up.sql is embedded into the CLI fresh bundle, so the
-- PREPARE hazard (cli_prepared_ddl.go) never reaches this file.
SET @has_memory_json = (
    SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'bd_events_journal'
      AND COLUMN_NAME = 'memory_json'
);
SET @sql = IF(@has_memory_json = 1,
    'ALTER TABLE bd_events_journal DROP COLUMN memory_json',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
