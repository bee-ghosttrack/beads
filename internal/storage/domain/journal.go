package domain

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
)

// MemoryJournalEntry is one memory-plane write for the journal, in the domain
// layer's own words: this package sits beneath issueops (which imports it for
// the config use case), so it cannot name issueops.EventMemory and the two ops
// directly. The db repository translates: Forget false is memory_remember,
// true is memory_forget; Key, Content and Previous are EventMemory's members
// with the same meaning (Content present on a remember, Previous present when
// a value existed before the write); Actor is as on every journal record, ""
// for none.
type MemoryJournalEntry struct {
	Forget   bool
	Key      string
	Content  *string
	Previous *string
	Actor    string
}

// EventsJournalSQLRepository is the transaction-bound persistence seam for
// reading and pruning the durable mutation journal, and for the ONE record the
// unit-of-work route has to write by hand: a memory-plane write.
//
// Every bead mutation journals itself at the issueops seam beneath the
// repositories, so no repository here records one. Memories are different:
// they are config rows, written through ConfigUseCase, which journals nothing
// (and must not — most config rows are settings, not memories). The memory
// body in internal/storage/uow therefore records its own write through this
// repository, on the same transaction runner the config write used, so the
// record and the row commit together exactly as a bead's do.
type EventsJournalSQLRepository interface {
	Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error)
	ReadPage(ctx context.Context, since int64, limit int) (storage.EventsJournalPage, error)
	Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error)
	// RecordMemoryEvent journals one memory_remember / memory_forget record in
	// this unit of work's transaction (issueops.RecordMemoryEventInTx). A
	// no-op while the journal is disabled.
	RecordMemoryEvent(ctx context.Context, entry MemoryJournalEntry) error
}

// EventsJournalUseCase exposes the bounded cursor operations needed by
// proxied-server callers without leaking a raw SQL connection.
type EventsJournalUseCase interface {
	Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error)
	// ReadPage is Read plus the journal head, for a consumer that must pace
	// itself rather than merely take what came next. See
	// issueops.ReadEventsPageInTx for why the head is not folded into Read.
	ReadPage(ctx context.Context, since int64, limit int) (storage.EventsJournalPage, error)
	Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error)
	// RecordMemoryEvent is the repository method of the same name, for the
	// memory body in internal/storage/uow. It is the one WRITE on this use
	// case, and it writes a journal record about a memory, never a bead.
	RecordMemoryEvent(ctx context.Context, entry MemoryJournalEntry) error
}

func NewEventsJournalUseCase(repo EventsJournalSQLRepository) EventsJournalUseCase {
	return &eventsJournalUseCase{repo: repo}
}

type eventsJournalUseCase struct {
	repo EventsJournalSQLRepository
}

var _ EventsJournalUseCase = (*eventsJournalUseCase)(nil)

func (u *eventsJournalUseCase) Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	return u.repo.Read(ctx, since, limit)
}

func (u *eventsJournalUseCase) ReadPage(ctx context.Context, since int64, limit int) (storage.EventsJournalPage, error) {
	return u.repo.ReadPage(ctx, since, limit)
}

func (u *eventsJournalUseCase) Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	return u.repo.Prune(ctx, before, retainDays, retainRows)
}

func (u *eventsJournalUseCase) RecordMemoryEvent(ctx context.Context, entry MemoryJournalEntry) error {
	return u.repo.RecordMemoryEvent(ctx, entry)
}
