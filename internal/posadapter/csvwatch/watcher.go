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
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

// bytesReader is a tiny helper to keep parseFile's signature unchanged
// (it takes an io.Reader). Using bytes.NewReader means the file is
// already in memory and closed by the time we call archive().
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Storage is the subset of the storage package's API the watcher needs.
// Defined here (consumer-side) to keep the storage package's surface
// small and to make the watcher trivially mockable in tests.
type Storage interface {
	WriteRawTransactions(ctx context.Context, batchID, posSourceID string, payloads [][]byte, hashes []string) (int, error)
}

// Watcher polls an fsnotify watch on the csvwatch folder, parses any
// new .csv files, writes their rows to storage, and moves the file to
// processed/ or failed/. One Watcher per folder.
type Watcher struct {
	storage    Storage
	normalizer Normalizer
	mart       Mart
	logger     *slog.Logger
	watcher    *fsnotify.Watcher
	// settleDelay is how long to wait after a CREATE/WRITE event before
	// reading the file. Most POS systems write the file in one shot,
	// but some append; this gives appending writers time to finish.
	settleDelay time.Duration
}

// Normalizer is the consumer-side interface the watcher needs from
// the normalizer package. Defined here to keep csvwatch decoupled
// from the normalizer's concrete type (and trivially mockable).
type Normalizer interface {
	Run(ctx context.Context, limit int) (processed, errors int, err error)
}

// Mart is the consumer-side interface the watcher needs from the
// mart package. Defined here for the same reason as Normalizer.
// Optional: a nil mart means the watcher skips the materialization
// step (useful in tests that only care about raw ingest).
type Mart interface {
	MaterializeAfterIngest(ctx context.Context) error
}

// NewWatcher creates a watcher. Call Start to begin processing. The
// mart argument may be nil for tests that don't care about the
// business data mart.
func NewWatcher(s Storage, n Normalizer, m Mart, logger *slog.Logger) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("csvwatch: new fsnotify watcher: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		storage:    s,
		normalizer: n,
		mart:       m,
		logger:     logger,
		watcher:    fw,
		settleDelay: 100 * time.Millisecond,
	}, nil
}

// Close releases the underlying fsnotify watcher.
func (w *Watcher) Close() error { return w.watcher.Close() }

// Start begins watching `folder` and processes any existing .csv files
// before returning. The watcher goroutine runs until ctx is done.
func (w *Watcher) Start(ctx context.Context, folder string) error {
	// Ensure the watch folder and its archive subdirs exist. We don't
	// trust the caller (operator could have deleted processed/ by hand)
	// and we don't want a missing subdir to turn every successful
	// ingest into an error.
	for _, d := range []string{folder, filepath.Join(folder, "processed"), filepath.Join(folder, "failed")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("csvwatch: mkdir %q: %w", d, err)
		}
	}
	if err := w.watcher.Add(folder); err != nil {
		return fmt.Errorf("csvwatch: watch %q: %w", folder, err)
	}
	if err := w.processExisting(ctx, folder); err != nil {
		w.logger.Warn("process existing files on startup", "folder", folder, "err", err)
	}
	go w.loop(ctx, folder)
	return nil
}

func (w *Watcher) loop(ctx context.Context, folder string) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			// Guard: skip directories and non-CSV files.
			info, err := os.Stat(ev.Name)
			if err != nil {
				if os.IsNotExist(err) {
					continue // already processed by an earlier event
				}
				w.logger.Error("stat failed", "path", ev.Name, "err", err)
				continue
			}
			if info.IsDir() {
				continue
			}
			if filepath.Ext(ev.Name) != ".csv" {
				continue
			}
			// Wait for the writer to finish. Most POS systems
			// open-write-close in one shot, but a slow POS could still
			// be writing. 100ms is enough on shop PCs in practice.
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

func (w *Watcher) processExisting(ctx context.Context, folder string) error {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".csv" {
			continue
		}
		if err := w.processFile(ctx, filepath.Join(folder, e.Name())); err != nil {
			w.logger.Error("process existing", "name", e.Name(), "err", err)
		}
	}
	return nil
}

// processFile is the workhorse: read → parse → write to storage →
// normalize → materialize → archive. Any file-level failure ends with
// the file in failed/; row-level failures in v1 (strict) also send
// the file to failed/. A successful write + normalize + materialize +
// archive is the only path to processed/.
//
// We read the whole file into memory (CSV files for v1 are small — a
// day's transactions fit in a few hundred KB) and then close the
// handle before renaming. On Windows, an open file handle blocks
// rename with "process cannot access the file".
//
// fsnotify often fires both CREATE and WRITE for a single dropped file
// (especially on Windows). The file may already have been processed
// and archived by an earlier event when this method is called a
// second time. We treat "file not found" on read as a no-op so the
// test logs stay clean and operators don't see spurious errors.
func (w *Watcher) processFile(ctx context.Context, path string) error {
	// Open, read everything into memory, then close. The file must be
	// closed before we attempt to rename/archive it — on Windows an
	// open handle blocks the rename with "process cannot access the
	// file". CSV files for v1 are small (a day's transactions fit in
	// a few hundred KB), so reading into memory is fine.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Already processed by an earlier event. Not an error.
			return nil
		}
		return w.archive(path, "failed", fmt.Errorf("open: %w", err))
	}

	rows, parseErrs, parseErr := parseFile(bytesReader(data))
	if parseErr != nil {
		return w.archive(path, "failed", parseErr)
	}
	if len(parseErrs) > 0 {
		for _, pe := range parseErrs {
			w.logger.Warn("csv row parse error",
				"file", filepath.Base(path),
				"line", pe.Line,
				"err", pe.Err,
			)
		}
		return w.archive(path, "failed",
			fmt.Errorf("%d row-level error(s)", len(parseErrs)))
	}
	if len(rows) == 0 {
		return w.archive(path, "failed", fmt.Errorf("no valid rows"))
	}

	// One batch per file, but write one row per pos_source so that
	// multi-lane CSVs preserve the source. We pre-compute hashes here
	// (cheap) and let the storage layer do the insert per source.
	batchID := uuid.NewString()
	bySource := make(map[string]int) // source -> index in batchGroups
	type group struct {
		payloads [][]byte
		hashes   []string
	}
	batchGroups := make([]group, 0, 4)
	for _, r := range rows {
		sum := sha256.Sum256(r.Payload)
		h := hex.EncodeToString(sum[:])
		if i, ok := bySource[r.PosSourceID]; ok {
			g := &batchGroups[i]
			g.payloads = append(g.payloads, r.Payload)
			g.hashes = append(g.hashes, h)
			continue
		}
		bySource[r.PosSourceID] = len(batchGroups)
		batchGroups = append(batchGroups, group{
			payloads: [][]byte{r.Payload},
			hashes:   []string{h},
		})
	}

	inserted := 0
	for source, i := range bySource {
		g := batchGroups[i]
		n, werr := w.storage.WriteRawTransactions(ctx, batchID, source, g.payloads, g.hashes)
		if werr != nil {
			return w.archive(path, "failed", fmt.Errorf("storage: %w", werr))
		}
		inserted += n
	}

	// 3. Normalize: convert the raw rows into canonical transactions.
	//    Run BEFORE archiving so the file stays in the drop folder if
	//    normalization fails (will be retried). Failures are logged but
	//    not fatal; the data is safe in raw and the next call (or the
	//    15-min mart timer) will retry.
	if w.normalizer != nil {
		processed, nerrs, nerr := w.normalizer.Run(ctx, 0)
		if nerr != nil {
			w.logger.Warn("normalize after ingest",
				"file", filepath.Base(path),
				"err", nerr,
			)
			// Don't archive — leave file for retry
			return fmt.Errorf("normalize: %w", nerr)
		} else {
			w.logger.Info("normalized",
				"file", filepath.Base(path),
				"processed", processed,
				"errors", nerrs,
			)
		}
	}

	// 4. Materialize: refresh the business data mart. Same idempotent
	//    UPSERT semantics as the normalizer. Phase 3 deliverable.
	if w.mart != nil {
		if merr := w.mart.MaterializeAfterIngest(ctx); merr != nil {
			w.logger.Warn("mart after ingest",
				"file", filepath.Base(path),
				"err", merr,
			)
			// Don't archive — leave file for retry
			return fmt.Errorf("materialize: %w", merr)
		}
	}

	// 5. Archive to processed/ only after successful normalize + materialize.
	if err := w.archive(path, "processed", nil); err != nil {
		// Data is in the DB and normalized; archive failed. Log but
		// don't fail the operation — the file will be re-processed on
		// next run and deduped via payload_hash.
		w.logger.Warn("archive after success", "err", err, "path", path)
		return nil
	}
	w.logger.Info("ingested file",
		"file", filepath.Base(path),
		"rows", len(rows),
		"inserted", inserted,
		"batch_id", batchID,
	)
	return nil
}

// archive moves the file to <dir>/<sub>/<basename>. If cause is non-nil
// it's logged at warn level (and returned). On a nil cause the file
// just moved and we return nil.
func (w *Watcher) archive(path, sub string, cause error) error {
	dst := filepath.Join(filepath.Dir(path), sub, filepath.Base(path))
	if err := os.Rename(path, dst); err != nil {
		wrapped := fmt.Errorf("archive to %q: %w", dst, err)
		if cause != nil {
			return fmt.Errorf("%w (after %v)", wrapped, cause)
		}
		return wrapped
	}
	if cause != nil {
		w.logger.Warn("archived to "+sub,
			"file", filepath.Base(path),
			"cause", cause.Error(),
		)
	}
	return cause
}
