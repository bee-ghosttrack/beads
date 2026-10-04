package db

import (
	"context"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

func NewEventsJournalSQLRepository(runner Runner) domain.EventsJournalSQLRepository {
	return &eventsJournalSQLRepository{runner: runner}
}

type eventsJournalSQLRepository struct {
	runner Runner
}

var _ domain.EventsJournalSQLRepository = (*eventsJournalSQLRepository)(nil)

func (r *eventsJournalSQLRepository) Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	return issueops.ReadEventsInTx(ctx, r.runner, since, limit)
}

func (r *eventsJournalSQLRepository) ReadPage(ctx context.Context, since int64, limit int) (storage.EventsJournalPage, error) {
	return issueops.ReadEventsPageInTx(ctx, r.runner, since, limit)
}

func (r *eventsJournalSQLRepository) Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	return issueops.PruneEventsInTx(ctx, r.runner, before, retainDays, retainRows, time.Now().UTC())
}

// RecordMemoryEvent journals a memory-plane write on this repository's runner —
// the unit of work's pinned transaction, so the record lands beside the config
// write it describes. The runner satisfies issueops.DBTX the same way the
// issue repository's does when it calls RecordEventInTx.
func (r *eventsJournalSQLRepository) RecordMemoryEvent(ctx context.Context, entry domain.MemoryJournalEntry) error {
	op := issueops.EventMemoryRemember
	if entry.Forget {
		op = issueops.EventMemoryForget
	}
	memory := &issueops.EventMemory{Key: entry.Key, Content: entry.Content, Previous: entry.Previous}
	return issueops.RecordMemoryEventInTx(ctx, r.runner, op, memory, entry.Actor)
}
