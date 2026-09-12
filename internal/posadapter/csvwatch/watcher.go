package csvwatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

// Storage is the subset of the storage package's API the watcher needs.
type Storage interface {
	WriteRawTransactions(ctx context.Context, batchID, posSourceID string, payloads [][]byte, hashes []string) (int, error)
}

// Normalizer is the consumer-side interface the watcher needs from the
// normalizer package.
type Normalizer interface {
	Run(ctx context.Context, limit int) (processed, errors int, err error)
}

// Mart is the consumer-side interface the watcher needs from the mart
// package. Optional: a nil mart skips materialization.
type Mart interface {
	MaterializeSince(ctx context.Context, since time.Time) error
}

// Watcher watches the csv_watch folder, parses new .csv files, writes
// their rows to storage, and moves the file to processed/ or failed/.
type Watcher struct {
	storage    Storage
	normalizer Normalizer
	mart       Mart
	logger     *slog.Logger
	watcher    *fsnotify.Watcher
	// settleDelay is how long to wait after a CREATE/WRITE event before
	// reading the file, giving appending writers time to finish.
	settleDelay time.Duration
}

// NewWatcher creates a watcher. Call Start to begin processing.
func NewWatcher(s Storage, n Normalizer, m Mart, logger *slog.Logger) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("csvwatch: new fsnotify watcher: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		storage:     s,
		normalizer:  n,
		mart:        m,
		logger:      logger,
		watcher:     fw,
		settleDelay: 250 * time.Millisecond,
	}, nil
}

// Close releases the underlying fsnotify watcher.
func (w *Watcher) Close() error { return w.watcher.Close() }

// Start begins watching `folder` and processes any existing .csv files
// before returning. The watcher goroutine runs until ctx is done.
func (w *Watcher) Start(ctx context.Context, folder string) error {
	for _, d := range []string{folder, filepath.Join(folder, "processed"), filepath.Join(folder, "failed")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("csvwatch: mkdir %q: %w", d, err)
		}
	}
	if err := w.watcher.Add(folder); err != nil {
		return fmt.Errorf("csvwatch: watch %q: %w", folder, err)
	}
	if err := w.ProcessExisting(ctx, folder); err != nil {
		w.logger.Warn("process existing files on startup", "folder", folder, "err", err)
	}
	go w.loop(ctx)
	return nil
}

func isCSV(name string) bool { return strings.EqualFold(filepath.Ext(name), ".csv") }

func (w *Watcher) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 || !isCSV(ev.Name) {
				continue
			}
			info, err := os.Stat(ev.Name)
			if err != nil || info.IsDir() {
				continue // already processed by an earlier event, or a folder
			}
			time.Sleep(w.settleDelay)
			if err := w.processFile(ctx, ev.Name); err != nil {
				w.logger.Error("process file failed", "path", ev.Name, "err", err)
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logger.Error("fsnotify error", "err", err)
		}
	}
}

// ProcessExisting ingests every .csv already in folder. It is also used
// by the periodic sweep so a file left behind (e.g. after a transient
// storage error) is retried without waiting for a new event.
func (w *Watcher) ProcessExisting(ctx context.Context, folder string) error {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isCSV(e.Name()) {
			continue
		}
		if err := w.processFile(ctx, filepath.Join(folder, e.Name())); err != nil {
			w.logger.Error("process existing", "name", e.Name(), "err", err)
		}
	}
	return nil
}

// processFile: read → parse → write raw → normalize → materialize →
// archive. The whole file is read into memory and the handle closed
// before renaming (an open handle blocks rename on Windows).
func (w *Watcher) processFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already processed by an earlier event
		}
		// Most likely the POS still has the file open; the next WRITE
		// event or the periodic sweep will retry.
		return fmt.Errorf("read: %w", err)
	}

	rows, parseErrs, parseErr := parseFile(bytes.NewReader(data))
	if parseErr != nil {
		return w.archive(path, "failed", parseErr)
	}
	if len(parseErrs) > 0 {
		for _, pe := range parseErrs {
			w.logger.Warn("csv row parse error", "file", filepath.Base(path), "line", pe.Line, "err", pe.Err)
		}
		return w.archive(path, "failed", fmt.Errorf("%d row-level error(s)", len(parseErrs)))
	}
	if len(rows) == 0 {
		return w.archive(path, "failed", fmt.Errorf("no valid rows"))
	}

	// One batch per file, grouped by pos_source_id so multi-lane CSVs
	// keep their source. Track the earliest business date for the mart.
	batchID := uuid.NewString()
	type group struct {
		payloads [][]byte
		hashes   []string
	}
	groups := map[string]*group{}
	var order []string
	earliest := rows[0].OccurredAt
	for _, r := range rows {
		if r.OccurredAt.Before(earliest) {
			earliest = r.OccurredAt
		}
		sum := sha256.Sum256(r.Payload)
		g, ok := groups[r.PosSourceID]
		if !ok {
			g = &group{}
			groups[r.PosSourceID] = g
			order = append(order, r.PosSourceID)
		}
		g.payloads = append(g.payloads, r.Payload)
		g.hashes = append(g.hashes, hex.EncodeToString(sum[:]))
	}

	inserted := 0
	for _, source := range order {
		g := groups[source]
		n, werr := w.storage.WriteRawTransactions(ctx, batchID, source, g.payloads, g.hashes)
		if werr != nil {
			// Leave the file in place: the data is not stored yet and a
			// retry (next event / sweep) must be able to pick it up.
			return fmt.Errorf("storage: %w", werr)
		}
		inserted += n
	}

	// The raw rows are durable from here on. Normalization or mart
	// failures are retried by the periodic refresh; the file can be
	// archived regardless.
	var normalized, nerrs int
	if w.normalizer != nil {
		var nerr error
		normalized, nerrs, nerr = w.normalizer.Run(ctx, 0)
		if nerr != nil {
			w.logger.Warn("normalize after ingest", "file", filepath.Base(path), "err", nerr)
		}
	}
	if w.mart != nil {
		since := earliest.In(time.Local)
		if merr := w.mart.MaterializeSince(ctx, since); merr != nil {
			w.logger.Warn("mart after ingest", "file", filepath.Base(path), "err", merr)
		}
	}

	if err := w.archive(path, "processed", nil); err != nil {
		w.logger.Warn("archive after success", "err", err, "path", path)
	}
	level := slog.LevelInfo
	if nerrs > 0 {
		level = slog.LevelWarn
	}
	w.logger.Log(ctx, level, "ingested file",
		"file", filepath.Base(path),
		"rows", len(rows),
		"inserted", inserted,
		"normalized", normalized,
		"unreadable_rows", nerrs,
		"batch_id", batchID,
	)
	return nil
}

// archive moves the file to <dir>/<sub>/<basename>. If a file with the
// same name is already archived (POS systems often reuse one export
// name), a timestamp is added so the earlier copy is never overwritten.
func (w *Watcher) archive(path, sub string, cause error) error {
	dst := uniqueArchivePath(filepath.Join(filepath.Dir(path), sub), filepath.Base(path))
	if err := os.Rename(path, dst); err != nil {
		wrapped := fmt.Errorf("archive to %q: %w", dst, err)
		if cause != nil {
			return fmt.Errorf("%w (after %v)", wrapped, cause)
		}
		return wrapped
	}
	if cause != nil {
		w.logger.Warn("archived to "+sub, "file", filepath.Base(dst), "cause", cause.Error())
	}
	return cause
}

func uniqueArchivePath(dir, name string) string {
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	stamp := time.Now().Format("20060102-150405")
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("%s_%s%s", stem, stamp, ext)
		if i > 0 {
			candidate = fmt.Sprintf("%s_%s-%d%s", stem, stamp, i, ext)
		}
		p := filepath.Join(dir, candidate)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}
