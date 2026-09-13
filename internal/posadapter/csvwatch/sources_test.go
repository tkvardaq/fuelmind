package csvwatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/storage"
)

const goodCSV = `pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method
lane_1,TX-001,2026-09-07T08:00:00Z,HSD,10,275.50,2755.00,CASH
lane_1,TX-002,2026-09-07T08:01:00Z,HSD,10,275.50,2755.00,CASH
`

// A station's POS writes where it writes. FuelMind has to be able to
// follow more than one folder — a lane per folder, or a second export
// from the back office — and to be told about a new one while running.
func TestWatchesSeveralFoldersAndPicksUpNewOnes(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	first, second := t.TempDir(), t.TempDir()
	if err := w.Watch(ctx, first); err != nil {
		t.Fatalf("Watch(first): %v", err)
	}
	if got := w.Folders(); len(got) != 1 {
		t.Fatalf("Folders() = %v, want one", got)
	}
	// Adding the same folder twice must not double anything up.
	if err := w.Watch(ctx, first); err != nil {
		t.Fatalf("Watch(first) again: %v", err)
	}
	if got := w.Folders(); len(got) != 1 {
		t.Errorf("Folders() = %v after adding the same folder twice, want one", got)
	}

	// A second folder added while the watcher is already running.
	if err := w.Watch(ctx, second); err != nil {
		t.Fatalf("Watch(second): %v", err)
	}
	writeFile(t, second, "day.csv", goodCSV)

	waitFor(t, 5*time.Second, func() bool { return countRaw(t, store) == 2 })
	if n := countRaw(t, store); n != 2 {
		t.Errorf("rows from the second folder = %d, want 2", n)
	}
}

// Reconcile is what the Data page calls: make what is watched match what
// the owner has configured, in one step.
func TestReconcileMatchesTheConfiguredFolders(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, b := t.TempDir(), t.TempDir()
	if errs := w.Reconcile(ctx, []string{a, b}); len(errs) != 0 {
		t.Fatalf("Reconcile reported errors for usable folders: %v", errs)
	}
	if got := w.Folders(); len(got) != 2 {
		t.Fatalf("Folders() = %v, want both", got)
	}

	// Dropping one leaves only the other.
	if errs := w.Reconcile(ctx, []string{a}); len(errs) != 0 {
		t.Fatalf("Reconcile: %v", errs)
	}
	got := w.Folders()
	if len(got) != 1 || !strings.EqualFold(got[0], a) {
		t.Errorf("Folders() = %v, want only %q", got, a)
	}

	// A folder that cannot be watched is reported rather than silently
	// dropped, and must not stop the good one from being watched.
	bad := "\x00not-a-path"
	errs := w.Reconcile(ctx, []string{a, bad})
	if len(errs) == 0 {
		t.Error("Reconcile accepted an unusable folder without reporting it")
	}
	if !containsFold(w.Folders(), a) {
		t.Errorf("the usable folder stopped being watched because another one failed: %v", w.Folders())
	}
}

// An upload goes through the same pipeline as a watched file, and hands
// back the rows that could not be read so the owner can fix them.
func TestIngestCSVFromAnUpload(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	ctx := context.Background()

	res, err := w.IngestCSV(ctx, "usb.csv", []byte(goodCSV))
	if err != nil {
		t.Fatalf("IngestCSV: %v", err)
	}
	if res.RowsInserted != 2 || res.RowsRejected != 0 {
		t.Errorf("res = %+v, want 2 inserted and none rejected", res)
	}
	if len(res.Rejects) != 0 {
		t.Errorf("a clean file produced a rejects file: %s", res.Rejects)
	}
	if n := countRaw(t, store); n != 2 {
		t.Errorf("stored %d rows, want 2", n)
	}

	// Re-uploading the same file must not double the day's takings.
	res, err = w.IngestCSV(ctx, "usb.csv", []byte(goodCSV))
	if err != nil {
		t.Fatalf("IngestCSV again: %v", err)
	}
	if res.RowsInserted != 0 {
		t.Errorf("re-uploading inserted %d rows, want 0", res.RowsInserted)
	}
	if n := countRaw(t, store); n != 2 {
		t.Errorf("after re-upload, stored %d rows, want still 2", n)
	}
}

func TestIngestCSVHandsBackTheRowsToFix(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)

	mixed := goodCSV + "lane_1,TX-BAD,NOT-A-DATE,HSD,10,275.50,2755.00,CASH\n"
	res, err := w.IngestCSV(context.Background(), "usb.csv", []byte(mixed))
	if err != nil {
		t.Fatalf("IngestCSV: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Errorf("inserted %d rows, want the 2 good ones", res.RowsInserted)
	}
	if res.RowsRejected != 1 {
		t.Errorf("rejected %d rows, want 1", res.RowsRejected)
	}
	rejects := string(res.Rejects)
	if !strings.Contains(rejects, "TX-BAD") || !strings.Contains(rejects, "fuelmind_error") {
		t.Errorf("rejects file does not carry the bad row and its reason:\n%s", rejects)
	}
	if strings.Contains(rejects, "TX-001") {
		t.Errorf("rejects file carries a good row too:\n%s", rejects)
	}
	if len(res.RejectReasons) != 1 {
		t.Errorf("RejectReasons = %v, want one line", res.RejectReasons)
	}
}

// A sale typed into the dashboard lands in the same tables an export
// does. That is the whole point: one pipeline, one set of figures.
func TestIngestRowsFromAManualSale(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)

	row, err := posadapter.BuildManualRow(posadapter.ManualSale{
		OccurredAt:     time.Now().Add(-time.Hour),
		ProductAlias:   "HSD",
		QuantityLiters: 20,
		UnitPrice:      275.50,
		PaymentMethod:  "CASH",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := w.IngestRows(context.Background(), "dashboard", []posadapter.RawTransaction{row})
	if err != nil {
		t.Fatalf("IngestRows: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Errorf("inserted %d, want 1", res.RowsInserted)
	}

	// Typed in twice by mistake, counted once.
	res, err = w.IngestRows(context.Background(), "dashboard", []posadapter.RawTransaction{row})
	if err != nil {
		t.Fatalf("IngestRows again: %v", err)
	}
	if res.RowsInserted != 0 {
		t.Errorf("the same sale entered twice inserted %d rows, want 0", res.RowsInserted)
	}
	if n := countRaw(t, store); n != 1 {
		t.Errorf("stored %d rows, want 1", n)
	}
}

// Everything that arrives is recorded, so the Data page can tell the
// owner what came in without them reading a log file.
func TestIngestIsRecorded(t *testing.T) {
	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	rec := &fakeRecorder{}
	w.SetRecorder(rec)

	if _, err := w.IngestCSV(context.Background(), "usb.csv", []byte(goodCSV)); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.source != sourceUpload || got.fileName != "usb.csv" || got.inserted != 2 || got.outcome != outcomeOK {
		t.Errorf("recorded %+v, want an ok upload of usb.csv with 2 rows", got)
	}

	// A file that cannot be read at all is recorded as a failure, which
	// is exactly the case the owner most needs to see.
	rec.calls = nil
	_, err := w.IngestCSV(context.Background(), "junk.csv", []byte("not,a,fuelmind,export\n1,2,3,4\n"))
	if err == nil {
		t.Fatal("a file with no FuelMind columns was accepted")
	}
	if len(rec.calls) != 1 || rec.calls[0].outcome != outcomeFailed {
		t.Errorf("recorded %+v, want one failed event", rec.calls)
	}
}

type recorded struct {
	source, origin, fileName string
	read, inserted, rejected int
	outcome, detail          string
}

type fakeRecorder struct{ calls []recorded }

func (f *fakeRecorder) RecordIngest(_ context.Context, source, origin, fileName string, read, inserted, rejected int, outcome, detail string) {
	f.calls = append(f.calls, recorded{source, origin, fileName, read, inserted, rejected, outcome, detail})
}

// countRaw is how many rows have reached the raw layer.
func countRaw(t *testing.T, store *storage.Storage) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM raw_pos_transactions`).Scan(&n); err != nil {
		t.Fatalf("count raw rows: %v", err)
	}
	return n
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
