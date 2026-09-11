package csvwatch

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/posadapter"
)

// requiredColumns is the v1 wire format. Order doesn't matter (we look
// up by name), but every column must be present or the file is
// rejected as a header error.
var requiredColumns = []string{
	"pos_source_id",
	"external_id",
	"occurred_at",
	"product_alias",
	"quantity_liters",
	"unit_price",
	"total_amount",
	"payment_method",
}

// ParseError is a non-fatal per-row error. One bad row is collected
// here; it doesn't kill the file. The Watcher decides at the file
// level whether to archive to processed/ or failed/ — in v1, any
// per-row error sends the file to failed/ (strict mode).
type ParseError struct {
	Line int    `json:"line"`
	Err  string `json:"err"`
}

func (e ParseError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Err)
}

// parseFile reads a CSV file from r and returns the parsed rows plus
// any per-row errors. A non-nil error from parseFile is a file-level
// failure (bad header, unreadable) and the file must go to failed/.
//
// A non-nil parseErrs slice is a row-level failure. In v1 the watcher
// treats this as a file failure too; a future "lenient" mode flag can
// relax this.
func parseFile(r io.Reader) (rows []posadapter.RawTransaction, rowErrs []ParseError, err error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	// FieldsPerRecord = -1 accepts variable-width rows; we validate by
	// header-name lookup so a missing trailing comma doesn't kill us.
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}

	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, req := range requiredColumns {
		if _, ok := col[req]; !ok {
			return nil, nil, fmt.Errorf("missing required column %q in header %v", req, header)
		}
	}

	lineNum := 1 // header consumed = line 1
	for {
		lineNum++
		rec, readErr := cr.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			rowErrs = append(rowErrs, ParseError{Line: lineNum, Err: readErr.Error()})
			continue
		}
		if len(rec) < len(header) {
			rowErrs = append(rowErrs, ParseError{
				Line: lineNum,
				Err:  fmt.Sprintf("row has %d columns, expected %d", len(rec), len(header)),
			})
			continue
		}

		occurredAtStr := strings.TrimSpace(rec[col["occurred_at"]])
		ts, terr := parseTimestamp(occurredAtStr)
		if terr != nil {
			rowErrs = append(rowErrs, ParseError{
				Line: lineNum,
				Err:  fmt.Sprintf("bad occurred_at %q: %v", occurredAtStr, terr),
			})
			continue
		}

		payload, jerr := csvRowToJSON(rec, header)
		if jerr != nil {
			rowErrs = append(rowErrs, ParseError{Line: lineNum, Err: fmt.Sprintf("json marshal: %v", jerr)})
			continue
		}

		rows = append(rows, posadapter.RawTransaction{
			PosSourceID: strings.TrimSpace(rec[col["pos_source_id"]]),
			ExternalID:  strings.TrimSpace(rec[col["external_id"]]),
			OccurredAt:  ts,
			Payload:     payload,
			PayloadKind: posadapter.PayloadJSON,
		})
	}

	return rows, rowErrs, nil
}

// csvRowToJSON turns one CSV row back into a JSON object keyed by
// header name. The normalizer (Phase 2) reads this payload and
// resolves product aliases, validates amounts, etc.
func csvRowToJSON(rec []string, header []string) ([]byte, error) {
	m := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(rec) {
			m[strings.TrimSpace(h)] = strings.TrimSpace(rec[i])
		}
	}
	return json.Marshal(m)
}

// parseTimestamp accepts the formats real-world POS systems actually
// emit. We try them in order of strictness; the first match wins.
//
//  1. Full RFC 3339 with timezone: "2026-09-07T08:14:22Z" or
//     "2026-09-07T08:14:22+05:00". The spec asks for this; some
//     modern POS systems emit it.
//  2. Naive RFC 3339 (no timezone): "2026-09-07T08:14:22". Most
//     regional fuel-station POSes — and Excel, and the typical
//     "today's date" export — emit this. We interpret it as the
//     station's local time and tag it as such.
//  3. Date + space + time: "2026-09-07 08:14:22". Some legacy
//     systems use a space instead of "T".
//
// Future improvement: read the station's IANA timezone from config and
// use it for naive timestamps. For v1 we leave the result in Local
// time; SQLite's CURRENT_TIMESTAMP and Go's time.Time handle the
// storage layer correctly regardless of zone.
func parseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	// 1. Full RFC 3339 with zone.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// 2 & 3. Naive forms — interpret in local time.
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format (expected RFC 3339 with or without timezone, e.g. 2026-09-07T08:14:22)")
}
