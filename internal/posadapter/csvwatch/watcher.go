package csvwatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"

	"github.com/fuelmind/fuelmind/internal/posadapter"
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

// Recorder is told about everything that arrives, so the dashboard can
// show the owner what came in and what happened to it. Optional.
type Recorder interface {
	RecordIngest(ctx context.Context, source, origin, fileName string, read, inserted, rejected int, outcome, detail string)
}

// Result is what one ingestion did. It is returned to the dashboard so
// an upload or a typed-in sale can say what happened, in numbers.
type Result struct {
	RowsRead     int
	RowsInserted int
	RowsRejected int
	// Rejects is the correction file the owner can fix and re-submit,
	// when some rows could not be read. Empty when every row was fine.
	Rejects []byte
	// RejectReasons is one line per bad row, for showing on screen.
	RejectReasons []string
}

// Ingest sources and outcomes. They mirror the storage package's
// constants without importing it, so csvwatch stays usable with any
// Storage implementation.
const (
	sourceFolder = "watch_folder"
	sourceUpload = "upload"
	sourceManual = "manual"

	outcomeOK      = "ok"
	outcomePartial = "partial"
	outcomeFailed  = "failed"
)

// Watcher watches one or more folders for CSV files dropped by a POS,
// parses them, writes their rows to storage, and moves each file to
// processed/ or failed/.
//
// It is also the single way data enters FuelMind: a file uploaded from
// the dashboard and a sale typed in by hand both go through the same
// parse, store, normalize and materialize path, so a figure can never
// depend on which door the data came in by.
type Watcher struct {
	storage    Storage
	normalizer Normalizer
	mart       Mart
	recorder   Recorder
	logger     *slog.Logger
	watcher    *fsnotify.Watcher
	// settleDelay is how long to wait after a CREATE/WRITE event before
	// reading the file, giving appending writers time to finish.
	settleDelay time.Duration

	mu       sync.Mutex
	folders  map[string]struct{}
	loopOnce sync.Once
}

// NewWatcher creates a watcher. Call Start or Watch to begin processing.
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
		folders:     map[string]struct{}{},
	}, nil
}

// SetRecorder attaches an ingest-history recorder.
func (w *Watcher) SetRecorder(r Recorder) { w.recorder = r }

// Close releases the underlying fsnotify watcher.
func (w *Watcher) Close() error { return w.watcher.Close() }

// Start begins watching folder and processes any existing .csv files
// before returning. The watcher goroutine runs until ctx is done.
func (w *Watcher) Start(ctx context.Context, folder string) error {
	return w.Watch(ctx, folder)
}

// Watch adds a folder to the watch set, creating its processed/ and
// failed/ sub-folders and sweeping whatever is already in it. Adding a
// folder that is already watched is a no-op, so it is safe to call on
// every configuration reload.
func (w *Watcher) Watch(ctx context.Context, folder string) error {
	clean := filepath.Clean(folder)

	w.mu.Lock()
	_, already := w.folders[clean]
	w.mu.Unlock()
	if already {
		return nil
	}

	for _, d := range []string{clean, filepath.Join(clean, "processed"), filepath.Join(clean, "failed")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("csvwatch: mkdir %q: %w", d, err)
		}
	}
	if err := w.watcher.Add(clean); err != nil {
		return fmt.Errorf("csvwatch: watch %q: %w", clean, err)
	}

	w.mu.Lock()
	w.folders[clean] = struct{}{}
	w.mu.Unlock()

	if err := w.ProcessExisting(ctx, clean); err != nil {
		w.logger.Warn("process existing files on startup", "folder", clean, "err", err)
	}
	w.loopOnce.Do(func() { go w.loop(ctx) })
	return nil
}

// Unwatch stops watching a folder. Files already ingested from it stay
// ingested; FuelMind simply stops looking there.
func (w *Watcher) Unwatch(folder string) error {
	clean := filepath.Clean(folder)
	w.mu.Lock()
	delete(w.folders, clean)
	w.mu.Unlock()
	if err := w.watcher.Remove(clean); err != nil && !strings.Contains(err.Error(), "non-existent") {
		return fmt.Errorf("csvwatch: unwatch %q: %w", clean, err)
	}
	return nil
}

// Folders lists the folders currently being watched.
func (w *Watcher) Folders() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.folders))
	for f := range w.folders {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Reconcile makes the watch set match want exactly: folders in want that
// are not watched are added, and watched folders not in want are
// dropped. It returns the error for each folder it could not add, keyed
// by path, rather than failing on the first one — one unreachable folder
// (a network share that is down, say) must not stop the others from
// being watched.
func (w *Watcher) Reconcile(ctx context.Context, want []string) map[string]error {
	wanted := map[string]struct{}{}
	for _, f := range want {
		wanted[filepath.Clean(f)] = struct{}{}
	}
	errs := map[string]error{}
	for _, have := range w.Folders() {
		if _, keep := wanted[have]; !keep {
			if err := w.Unwatch(have); err != nil {
				w.logger.Warn("stop watching folder", "folder", have, "err", err)
			} else {
				w.logger.Info("stopped watching folder", "folder", have)
			}
		}
	}
	for f := range wanted {
		if err := w.Watch(ctx, f); err != nil {
			errs[f] = err
			w.logger.Warn("cannot watch folder", "folder", f, "err", err)
		}
	}
	return errs
}

// Sweep re-reads every watched folder. The periodic refresh calls it so
// nothing depends on a file-system event arriving.
func (w *Watcher) Sweep(ctx context.Context) {
	for _, folder := range w.Folders() {
		if err := w.ProcessExisting(ctx, folder); err != nil {
			w.logger.Warn("drop-folder sweep", "folder", folder, "err", err)
		}
	}
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

// processFile: read, parse, write raw, normalize, materialize, archive.
// The whole file is read into memory and the handle closed before
// renaming (an open handle blocks rename on Windows).
func (w *Watcher) processFile(ctx context.Context, path string) error {
	folder := filepath.Dir(path)
	name := filepath.Base(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already processed by an earlier event
		}
		// Most likely the POS still has the file open; the next WRITE
		// event or the periodic sweep will retry.
		return fmt.Errorf("read: %w", err)
	}

	rows, parseErrs, header, parseErr := parseFile(bytes.NewReader(data))
	if parseErr != nil {
		// A file-level problem (no header, or a missing required column)
		// means no row in the file can be trusted.
		w.record(ctx, sourceFolder, folder, name, 0, 0, 0, outcomeFailed, parseErr.Error())
		return w.archive(path, "failed", parseErr)
	}
	for _, pe := range parseErrs {
		w.logger.Warn("csv row parse error", "file", name, "line", pe.Line, "err", pe.Err)
	}
	if len(rows) == 0 {
		detail := fmt.Sprintf("no readable rows (%d row error(s))", len(parseErrs))
		w.record(ctx, sourceFolder, folder, name, 0, 0, len(parseErrs), outcomeFailed, detail)
		return w.archive(path, "failed", fmt.Errorf("%s", detail))
	}
	// Some rows are bad, most are not. Losing a day of real sales because
	// one row has a typo is worse than either alternative, so the good
	// rows go in and the bad ones are written out on their own for the
	// owner to correct and drop back in.
	if len(parseErrs) > 0 {
		if err := w.writeRejects(path, header, parseErrs); err != nil {
			w.logger.Error("could not write the rejected rows", "file", name, "err", err)
		}
	}

	inserted, earliest, batchID, err := w.storeRows(ctx, rows)
	if err != nil {
		// Leave the file in place: the data is not stored yet and a
		// retry (next event / sweep) must be able to pick it up.
		return err
	}
	normalized, nerrs := w.settle(ctx, earliest, name)

	if err := w.archive(path, "processed", nil); err != nil {
		w.logger.Warn("archive after success", "err", err, "path", path)
	}
	level := slog.LevelInfo
	outcome, detail := outcomeOK, ""
	if nerrs > 0 || len(parseErrs) > 0 {
		level = slog.LevelWarn
		outcome = outcomePartial
		detail = fmt.Sprintf("%d row(s) rejected, %d row(s) could not be read", len(parseErrs), nerrs)
	}
	w.record(ctx, sourceFolder, folder, name, len(rows), inserted, len(parseErrs), outcome, detail)
	w.logger.Log(ctx, level, "ingested file",
		"file", name,
		"rows", len(rows),
		"inserted", inserted,
		"normalized", normalized,
		"rejected_rows", len(parseErrs),
		"unreadable_rows", nerrs,
		"batch_id", batchID,
	)
	return nil
}

// IngestCSV ingests CSV bytes that did not come from a watch folder — a
// file the owner uploaded from the dashboard. Nothing is written to
// disk: the rows go straight into the same pipeline, and any rows that
// could not be read come back in Result.Rejects as a CSV the owner can
// correct and upload again.
func (w *Watcher) IngestCSV(ctx context.Context, fileName string, data []byte) (Result, error) {
	rows, parseErrs, header, parseErr := parseFile(bytes.NewReader(data))
	if parseErr != nil {
		w.record(ctx, sourceUpload, "", fileName, 0, 0, 0, outcomeFailed, parseErr.Error())
		return Result{}, parseErr
	}

	res := Result{RowsRead: len(rows), RowsRejected: len(parseErrs)}
	for _, pe := range parseErrs {
		res.RejectReasons = append(res.RejectReasons, pe.Error())
	}
	if len(parseErrs) > 0 {
		if buf, err := rejectsCSV(header, parseErrs); err == nil {
			res.Rejects = buf
		} else {
			w.logger.Warn("could not build the rejects file", "file", fileName, "err", err)
		}
	}
	if len(rows) == 0 {
		detail := fmt.Sprintf("no readable rows (%d row error(s))", len(parseErrs))
		w.record(ctx, sourceUpload, "", fileName, 0, 0, len(parseErrs), outcomeFailed, detail)
		return res, fmt.Errorf("%s", detail)
	}

	inserted, earliest, _, err := w.storeRows(ctx, rows)
	if err != nil {
		w.record(ctx, sourceUpload, "", fileName, len(rows), 0, len(parseErrs), outcomeFailed, err.Error())
		return res, err
	}
	res.RowsInserted = inserted
	_, nerrs := w.settle(ctx, earliest, fileName)

	outcome, detail := outcomeOK, ""
	if nerrs > 0 || len(parseErrs) > 0 {
		outcome = outcomePartial
		detail = fmt.Sprintf("%d row(s) rejected, %d row(s) could not be read", len(parseErrs), nerrs)
	}
	w.record(ctx, sourceUpload, "", fileName, len(rows), inserted, len(parseErrs), outcome, detail)
	w.logger.Info("ingested upload", "file", fileName,
		"rows", len(rows), "inserted", inserted, "rejected_rows", len(parseErrs))
	return res, nil
}

// IngestRows puts already-built rows into the pipeline. It is how a sale
// typed into the dashboard reaches the same tables a POS export does,
// rather than through a second code path that could disagree with the
// first.
func (w *Watcher) IngestRows(ctx context.Context, origin string, rows []posadapter.RawTransaction) (Result, error) {
	if len(rows) == 0 {
		return Result{}, fmt.Errorf("csvwatch: nothing to ingest")
	}
	inserted, earliest, _, err := w.storeRows(ctx, rows)
	if err != nil {
		w.record(ctx, sourceManual, origin, "", len(rows), 0, 0, outcomeFailed, err.Error())
		return Result{}, err
	}
	_, nerrs := w.settle(ctx, earliest, origin)

	outcome, detail := outcomeOK, ""
	if nerrs > 0 {
		outcome, detail = outcomePartial, fmt.Sprintf("%d row(s) could not be read", nerrs)
	}
	w.record(ctx, sourceManual, origin, "", len(rows), inserted, 0, outcome, detail)
	return Result{RowsRead: len(rows), RowsInserted: inserted}, nil
}

// storeRows writes raw rows, grouped by pos_source_id so multi-lane
// exports keep their source. It returns how many rows were new (the
// unique payload hash makes a re-import a no-op), the earliest business
// date touched, and the batch id.
func (w *Watcher) storeRows(ctx context.Context, rows []posadapter.RawTransaction) (inserted int, earliest time.Time, batchID string, err error) {
	batchID = uuid.NewString()
	type group struct {
		payloads [][]byte
		hashes   []string
	}
	groups := map[string]*group{}
	var order []string
	earliest = rows[0].OccurredAt
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
	for _, source := range order {
		g := groups[source]
		n, werr := w.storage.WriteRawTransactions(ctx, batchID, source, g.payloads, g.hashes)
		if werr != nil {
			return 0, earliest, batchID, fmt.Errorf("storage: %w", werr)
		}
		inserted += n
	}
	return inserted, earliest, batchID, nil
}

// settle runs normalization and mart materialization after new raw rows
// land. The raw rows are durable by this point, so a failure here is a
// warning: the periodic refresh retries it.
func (w *Watcher) settle(ctx context.Context, earliest time.Time, origin string) (normalized, unreadable int) {
	if w.normalizer != nil {
		var nerr error
		normalized, unreadable, nerr = w.normalizer.Run(ctx, 0)
		if nerr != nil {
			w.logger.Warn("normalize after ingest", "origin", origin, "err", nerr)
		}
	}
	if w.mart != nil {
		if merr := w.mart.MaterializeSince(ctx, earliest.In(time.Local)); merr != nil {
			w.logger.Warn("mart after ingest", "origin", origin, "err", merr)
		}
	}
	return normalized, unreadable
}

// record appends to the ingest history, when one is attached.
func (w *Watcher) record(ctx context.Context, source, origin, fileName string, read, inserted, rejected int, outcome, detail string) {
	if w.recorder == nil {
		return
	}
	w.recorder.RecordIngest(ctx, source, origin, fileName, read, inserted, rejected, outcome, detail)
}

// rejectsCSV renders the rows that could not be read as a CSV in the
// same format as the input, with a fuelmind_error column explaining each
// one. Fixing those rows and re-submitting the file is all it takes.
func rejectsCSV(header []string, rowErrs []ParseError) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	if err := cw.Write(append(append([]string{}, header...), "fuelmind_error")); err != nil {
		return nil, err
	}
	for _, pe := range rowErrs {
		// Pad or trim the row to the header width so the file stays
		// rectangular even when the bad row was the wrong shape.
		row := make([]string, len(header), len(header)+1)
		copy(row, pe.Record)
		if err := cw.Write(append(row, fmt.Sprintf("line %d: %s", pe.Line, pe.Err))); err != nil {
			return nil, err
		}
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// writeRejects writes the rows that could not be read to
// failed/<name>.rejected.csv, next to the file they came from.
func (w *Watcher) writeRejects(srcPath string, header []string, rowErrs []ParseError) error {
	dir := filepath.Join(filepath.Dir(srcPath), "failed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Base(srcPath)
	ext := filepath.Ext(base)
	dst := uniqueArchivePath(dir, strings.TrimSuffix(base, ext)+".rejected"+ext)

	data, err := rejectsCSV(header, rowErrs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	w.logger.Warn("rows written for correction",
		"file", base, "rejected", len(rowErrs), "rejects_file", filepath.Base(dst))
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
