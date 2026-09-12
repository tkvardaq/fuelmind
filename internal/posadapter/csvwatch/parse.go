package csvwatch

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/normalizer"
	"github.com/fuelmind/fuelmind/internal/posadapter"
)

// requiredColumns is the v1 wire format. Order doesn't matter (we look
// up by name, case-insensitively), but every column must be present or
// the file is rejected as a header error.
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

// ParseError is a non-fatal per-row error. In v1 any per-row error sends
// the whole file to failed/ (strict mode).
type ParseError struct {
	Line int    `json:"line"`
	Err  string `json:"err"`
}

func (e ParseError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Err)
}

// parseFile reads a CSV file from r and returns the parsed rows plus
// any per-row errors. A non-nil error is a file-level failure (bad
// header, unreadable).
func parseFile(r io.Reader) (rows []posadapter.RawTransaction, rowErrs []ParseError, err error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	// Normalize header names: Excel's "CSV UTF-8" adds a byte-order mark
	// to the first cell, and some POS systems upper-case column names.
	for i, h := range header {
		header[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, string(rune(0xFEFF)))))
	}

	col := make(map[string]int, len(header))
	for i, h := range header {
		col[h] = i
	}
	for _, req := range requiredColumns {
		if _, ok := col[req]; !ok {
			return nil, nil, fmt.Errorf("missing required column %q in header %v", req, header)
		}
	}

	lineNum := 1
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
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // blank line
		}
		if len(rec) < len(header) {
			rowErrs = append(rowErrs, ParseError{
				Line: lineNum,
				Err:  fmt.Sprintf("row has %d columns, expected %d", len(rec), len(header)),
			})
			continue
		}
		source := strings.TrimSpace(rec[col["pos_source_id"]])
		if source == "" {
			rowErrs = append(rowErrs, ParseError{Line: lineNum, Err: "pos_source_id is empty"})
			continue
		}

		occurredAtStr := strings.TrimSpace(rec[col["occurred_at"]])
		ts, terr := normalizer.ParseTimestamp(occurredAtStr, time.Local)
		if terr != nil {
			rowErrs = append(rowErrs, ParseError{
				Line: lineNum,
				Err:  fmt.Sprintf("bad occurred_at %q: expected RFC 3339 with or without time zone, e.g. 2026-09-07T08:14:22", occurredAtStr),
			})
			continue
		}

		payload, jerr := csvRowToJSON(rec, header)
		if jerr != nil {
			rowErrs = append(rowErrs, ParseError{Line: lineNum, Err: fmt.Sprintf("json marshal: %v", jerr)})
			continue
		}

		rows = append(rows, posadapter.RawTransaction{
			PosSourceID: source,
			ExternalID:  strings.TrimSpace(rec[col["external_id"]]),
			OccurredAt:  ts,
			Payload:     payload,
			PayloadKind: posadapter.PayloadJSON,
		})
	}

	return rows, rowErrs, nil
}

// csvRowToJSON turns one CSV row into a JSON object keyed by the
// (normalized) header name.
func csvRowToJSON(rec []string, header []string) ([]byte, error) {
	m := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(rec) {
			m[h] = strings.TrimSpace(rec[i])
		}
	}
	return json.Marshal(m)
}
