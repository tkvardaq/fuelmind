package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/phone"
	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// Ingester is what the Data page needs from the POS watcher: the ability
// to take a file or a typed-in sale, and to change which folders are
// being watched. Nil means this build cannot accept data from the
// dashboard, and the page says so rather than offering buttons that do
// nothing.
type Ingester interface {
	IngestCSV(ctx context.Context, fileName string, data []byte) (csvwatch.Result, error)
	IngestRows(ctx context.Context, origin string, rows []posadapter.RawTransaction) (csvwatch.Result, error)
	Reconcile(ctx context.Context, want []string) map[string]error
	Folders() []string
}

// maxUploadBytes caps an uploaded export. A day's transactions for a busy
// station is a few hundred kilobytes; 32 MB is generous and still keeps a
// mis-chosen file from filling memory.
const maxUploadBytes = 32 << 20

// handleData is where the owner says where their data comes from, puts a
// file in by hand, types a sale in, and sees what has actually arrived.
//
// FuelMind reads what another system exports rather than talking to the
// pumps, so this page is the whole answer to "how does my data get in?".
func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	var formError, notice string

	if r.Method == http.MethodPost {
		if s.Ingest == nil {
			formError = "This build cannot accept data from the dashboard."
		} else {
			switch r.FormValue("action") {
			case "add_folder":
				notice, formError = s.addWatchFolder(r)
			case "folder_enabled":
				notice, formError = s.setWatchFolderEnabled(r)
			case "remove_folder":
				notice, formError = s.removeWatchFolder(r)
			case "upload":
				notice, formError = s.uploadExport(w, r)
				if notice == "" && formError == "" {
					return // the rejects file was sent as a download
				}
			case "manual_sale":
				notice, formError = s.recordManualSale(r)
			default:
				formError = "Unknown action."
			}
		}
	}

	folders, err := s.store.WatchFolders(r.Context())
	if err != nil {
		s.renderError(w, "data: folders", err)
		return
	}
	events, err := s.store.RecentIngestEvents(r.Context(), 20)
	if err != nil {
		s.renderError(w, "data: history", err)
		return
	}

	// Which folders are actually being watched right now, as opposed to
	// which ones are configured. They can differ: a network share that
	// is down is configured but not watched, and the owner needs to see
	// that difference rather than assume data is flowing.
	watching := map[string]bool{}
	if s.Ingest != nil {
		for _, f := range s.Ingest.Folders() {
			watching[f] = true
		}
	}
	type folderView struct {
		storage.WatchFolder
		Live bool
	}
	views := make([]folderView, 0, len(folders))
	for _, f := range folders {
		views = append(views, folderView{WatchFolder: f, Live: watching[f.Path]})
	}

	lastIngest := ""
	if len(events) > 0 {
		lastIngest = events[0].OccurredAt.Local().Format("Mon 2 Jan 15:04")
	}

	s.render(w, r, "data", map[string]any{
		"LoggedIn":   true,
		"Folders":    views,
		"Events":     events,
		"LastIngest": lastIngest,
		"Products":   s.productAliases(r.Context()),
		"CanIngest":  s.Ingest != nil,
		"Today":      time.Now().Format("2006-01-02"),
		"Now":        time.Now().Format("15:04"),
		"Error":      formError,
		"Notice":     notice,
	})
}

// productAliases lists the products a manual sale can be recorded
// against, taken from the same table the normalizer resolves against so
// the dropdown can never offer something that would then fail to
// normalize.
func (s *Server) productAliases(ctx context.Context) []string {
	aliases, err := s.store.ProductAliases(ctx)
	if err != nil {
		s.logger.Warn("data: load product aliases", "err", err)
		return []string{"DIESEL", "PETROL_92", "PETROL_95"}
	}
	seen := map[string]bool{}
	var out []string
	for _, code := range aliases {
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	if len(out) == 0 {
		return []string{"DIESEL", "PETROL_92", "PETROL_95"}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// addWatchFolder points FuelMind at a folder the POS already writes to,
// so nobody has to copy files by hand.
func (s *Server) addWatchFolder(r *http.Request) (notice, formError string) {
	path := strings.TrimSpace(r.FormValue("path"))
	label := strings.TrimSpace(r.FormValue("label"))
	if err := s.store.AddWatchFolder(r.Context(), path, label); err != nil {
		return "", friendlyFolderError(err)
	}
	s.applyWatchFolders(r.Context())
	s.audit(r, storage.ActionFolderAdded, path, fmt.Sprintf("Started watching %s for POS exports", path))
	return fmt.Sprintf("Now watching %s. Exports dropped there are read automatically.", path), ""
}

func (s *Server) setWatchFolderEnabled(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return "", "That folder no longer exists."
	}
	on := r.FormValue("enabled") == "on"
	if err := s.store.SetWatchFolderEnabled(r.Context(), id, on); err != nil {
		return "", err.Error()
	}
	s.applyWatchFolders(r.Context())
	state := "off"
	if on {
		state = "on"
	}
	s.audit(r, storage.ActionFolderChanged, folderPath(r.Context(), s, id),
		"Switched a watched folder "+state)
	if on {
		return "Folder switched on.", ""
	}
	return "Folder switched off. Nothing there will be read until you switch it back on.", ""
}

func (s *Server) removeWatchFolder(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return "", "That folder no longer exists."
	}
	// Read the path before the row goes, so the log says which folder.
	removed := folderPath(r.Context(), s, id)
	if err := s.store.RemoveWatchFolder(r.Context(), id); err != nil {
		return "", err.Error()
	}
	s.applyWatchFolders(r.Context())
	s.audit(r, storage.ActionFolderRemoved, removed, "Stopped watching "+removed)
	return "Folder removed. Sales already read from it are still here.", ""
}

// applyWatchFolders makes the running watcher match what is configured.
// A folder that cannot be watched right now is recorded against the
// folder so the page can show why, instead of failing silently.
func (s *Server) applyWatchFolders(ctx context.Context) {
	if s.Ingest == nil {
		return
	}
	want, err := s.store.EnabledWatchFolders(ctx)
	if err != nil {
		s.logger.Warn("data: read watch folders", "err", err)
		return
	}
	failures := s.Ingest.Reconcile(ctx, want)
	for _, path := range want {
		msg := ""
		if err, bad := failures[path]; bad {
			msg = err.Error()
		}
		if err := s.store.SetWatchFolderError(ctx, path, msg); err != nil {
			s.logger.Warn("data: record folder error", "folder", path, "err", err)
		}
	}
}

// ApplyWatchFolders makes the running watcher match the folders the owner
// has configured. The core calls it at startup; the Data page calls it
// again whenever a folder is added, switched or removed.
func (s *Server) ApplyWatchFolders(ctx context.Context) { s.applyWatchFolders(ctx) }

// friendlyFolderError keeps the storage layer's explanation but drops the
// package prefix, so the owner reads a sentence rather than a Go error.
func friendlyFolderError(err error) string {
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, "folder is not usable: "); ok {
		return strings.ToUpper(rest[:1]) + rest[1:]
	}
	return msg
}

// uploadExport takes a CSV straight from the browser, for the owner who
// has an export on a USB stick or in their email rather than in a folder
// on this PC. When some rows cannot be read, the corrected-rows file is
// sent back as a download instead of a page.
func (s *Server) uploadExport(w http.ResponseWriter, r *http.Request) (notice, formError string) {
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		return "", "That file could not be read. It may be larger than 32 MB."
	}
	file, header, err := r.FormFile("export")
	if err != nil {
		return "", "Choose a CSV file to import."
	}
	defer file.Close()

	data := make([]byte, 0, 64<<10)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := file.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > maxUploadBytes {
			return "", "That file is larger than 32 MB."
		}
		if rerr != nil {
			break
		}
	}

	res, err := s.Ingest.IngestCSV(r.Context(), header.Filename, data)
	if err != nil {
		return "", fmt.Sprintf("%s could not be imported: %v", header.Filename, err)
	}
	s.refreshMart(r)

	if len(res.Rejects) > 0 {
		// Hand the bad rows straight back, so the owner can fix them in
		// the same spreadsheet program they came from and upload again.
		name := strings.TrimSuffix(header.Filename, ".csv") + "-rows-to-fix.csv"
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFileName(name)+"\"")
		w.Header().Set("X-FuelMind-Imported", strconv.Itoa(res.RowsInserted))
		_, _ = w.Write(res.Rejects)
		return "", ""
	}
	s.audit(r, storage.ActionFileImported, header.Filename,
		fmt.Sprintf("Imported %s by hand: %d sale(s) added", header.Filename, res.RowsInserted))
	return fmt.Sprintf("Imported %s: %d sale(s) added.", header.Filename, res.RowsInserted), ""
}

// sanitizeFileName keeps a downloaded name free of characters that would
// break the Content-Disposition header.
func sanitizeFileName(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '/', '\r', '\n':
			return '-'
		}
		return r
	}, name)
}

// recordManualSale puts one sale in by hand, for when the POS is down or
// a pump was written up in the day book.
func (s *Server) recordManualSale(r *http.Request) (notice, formError string) {
	date := strings.TrimSpace(r.FormValue("date"))
	clock := strings.TrimSpace(r.FormValue("time"))
	if clock == "" {
		clock = "12:00"
	}
	when, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, time.Local)
	if err != nil {
		return "", "Choose a valid date and time for the sale."
	}

	sale := posadapter.ManualSale{
		OccurredAt:     when,
		ProductAlias:   strings.TrimSpace(r.FormValue("product")),
		QuantityLiters: parseAmount(r.FormValue("liters")),
		UnitPrice:      parseAmount(r.FormValue("unit_price")),
		TotalAmount:    parseAmount(r.FormValue("total")),
		PaymentMethod:  strings.TrimSpace(r.FormValue("payment_method")),
		CustomerPhone:  strings.TrimSpace(r.FormValue("customer_phone")),
		PumpID:         strings.TrimSpace(r.FormValue("pump_id")),
		Attendant:      strings.TrimSpace(r.FormValue("attendant")),
		Note:           strings.TrimSpace(r.FormValue("note")),
		EnteredBy:      "dashboard",
	}
	row, err := posadapter.BuildManualRow(sale)
	if err != nil {
		return "", err.Error()
	}

	res, err := s.Ingest.IngestRows(r.Context(), "dashboard", []posadapter.RawTransaction{row})
	if err != nil {
		return "", fmt.Sprintf("The sale could not be saved: %v", err)
	}
	s.refreshMart(r)
	if res.RowsInserted == 0 {
		return "", "That exact sale is already recorded, so nothing was added."
	}
	s.audit(r, storage.ActionSaleEntered, row.ExternalID, fmt.Sprintf(
		"Entered a sale by hand: %s litres of %s at %s per litre, paid by %s%s",
		strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", sale.QuantityLiters), "0"), "."),
		sale.ProductAlias, fmt.Sprintf("%.2f", sale.UnitPrice), strings.ToLower(sale.PaymentMethod),
		creditSuffix(sale.CustomerPhone)))
	return "Sale recorded. Today's figures have been updated.", ""
}

// parseAmount reads a number the way a person types it, tolerating
// thousands separators and stray spaces. An unreadable value returns 0,
// which the sale validation then rejects with a useful message.
func parseAmount(s string) float64 {
	clean := strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if clean == "" {
		return 0
	}
	v, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0
	}
	return v
}

// folderPath names a watch folder for the activity log, falling back to
// its id when the row has already gone.
func folderPath(ctx context.Context, s *Server, id int64) string {
	folders, err := s.store.WatchFolders(ctx)
	if err == nil {
		for _, f := range folders {
			if f.ID == id {
				return f.Path
			}
		}
	}
	return fmt.Sprintf("folder %d", id)
}

// creditSuffix names the customer on a credit sale, because a credit
// sale entered by hand puts money on someone's account.
func creditSuffix(customer string) string {
	if strings.TrimSpace(customer) == "" {
		return ""
	}
	return " for " + phone.Normalize(customer)
}
