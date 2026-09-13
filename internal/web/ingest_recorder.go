package web

import (
	"context"
	"log/slog"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// IngestRecorder writes the ingest history the Data page reads back. It
// adapts the watcher's Recorder interface to storage, so csvwatch does
// not have to know about the storage package.
type IngestRecorder struct {
	Store  *storage.Storage
	Logger *slog.Logger
}

// RecordIngest appends one line to the history. A failure to record is
// logged and swallowed: losing the audit line must never cost the data
// that was just ingested.
func (r IngestRecorder) RecordIngest(ctx context.Context, source, origin, fileName string, read, inserted, rejected int, outcome, detail string) {
	if r.Store == nil {
		return
	}
	err := r.Store.RecordIngestEvent(ctx, storage.IngestEvent{
		Source:       source,
		Origin:       origin,
		FileName:     fileName,
		RowsRead:     read,
		RowsInserted: inserted,
		RowsRejected: rejected,
		Outcome:      outcome,
		Detail:       detail,
	})
	if err != nil && r.Logger != nil {
		r.Logger.Warn("could not record what arrived", "source", source, "err", err)
	}
}
