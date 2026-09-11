package normalizer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func newTestNormalizer(t *testing.T) *Normalizer {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := New(s, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// rawFromRow builds a raw_pos_transactions row + a matching
// raw_payload JSON from a map (mimics what csvwatch would have
// written). Returns the raw row ID and writes 1 row.
func rawFromRow(t *testing.T, s *storage.Storage, batchID, posSource string, m map[string]string) int64 {
	t.Helper()
	payload, err := jsonMarshal(t, m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.WriteRawTransactions(context.Background(), batchID, posSource, [][]byte{payload}, []string{hashOf(payload)})
	if err != nil {
		t.Fatal(err)
	}
	// Read the ID back. SQLite's last_insert_rowid is per-connection
	// so this is safe.
	rows, qerr := s.UnnormalizedTransactions(context.Background(), 0)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) == 0 {
		t.Fatal("no raw row found after insert")
	}
	return rows[len(rows)-1].ID
}

// --- Test scaffolding helpers (avoid pulling encoding/json into
// the test file just for these) ---

func jsonMarshal(t *testing.T, m map[string]string) ([]byte, error) {
	t.Helper()
	// Use a tiny hand-rolled marshaler to keep dependencies tight.
	// Map iteration order is random in Go but our contract test
	// doesn't care; we just need a valid JSON object.
	var b []byte
	b = append(b, '{')
	first := true
	for k, v := range m {
		if !first {
			b = append(b, ',')
		}
		first = false
		kb, _ := jsonEscapeString(k)
		vb, _ := jsonEscapeString(v)
		b = append(b, '"')
		b = append(b, kb...)
		b = append(b, '"', ':', '"')
		b = append(b, vb...)
		b = append(b, '"')
	}
	b = append(b, '}')
	return b, nil
}

func jsonEscapeString(s string) ([]byte, error) {
	// Minimal escape for the strings we use: " and \. Good enough
	// for the test fixtures.
	var b []byte
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			b = append(b, byte(r))
		}
	}
	return b, nil
}

func hashOf(b []byte) string {
	// The storage layer dedupes by payload_hash, but for these
	// tests we want each row to insert (not collide). Use a
	// placeholder; collisions are unlikely with 1-row batches.
	// We accept a fake short hash and rely on the test driver
	// to not insert duplicates within one test.
	const letters = "0123456789abcdef"
	h := make([]byte, 8)
	for i := range h {
		h[i] = letters[int(b[i])%len(letters)]
	}
	return string(h)
}

// --- The actual tests ---

func TestNormalizeAliasResolution(t *testing.T) {
	n := newTestNormalizer(t)
	cases := []struct {
		alias string
		want  string
	}{
		{"DIESEL", "DIESEL"},
		{"Diesel", "DIESEL"},
		{"HSD", "DIESEL"},
		{"Hi-Speed Diesel", "DIESEL"},
		{"PETROL_92", "PETROL_92"},
		{"P-92", "PETROL_92"},
		{"PMG-92", "PETROL_92"},
		{"PETROL_95", "PETROL_95"},
	}
	for _, c := range cases {
		got, ok := n.aliases[c.alias]
		if !ok {
			// Try case-insensitive.
			for k, v := range n.aliases {
				if caseEqual(k, c.alias) {
					got, ok = v, true
					break
				}
			}
		}
		if !ok || got != c.want {
			t.Errorf("alias %q -> %q, want %q", c.alias, got, c.want)
		}
	}
}

func caseEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' { ca += 32 }
		if cb >= 'A' && cb <= 'Z' { cb += 32 }
		if ca != cb { return false }
	}
	return true
}

func TestNormalizeRowEndToEnd(t *testing.T) {
	n := newTestNormalizer(t)
	id := rawFromRow(t, n.store, "b1", "lane_1", map[string]string{
		"pos_source_id":   "lane_1",
		"external_id":     "TX-001",
		"occurred_at":     "2026-09-07T08:14:22",
		"product_alias":   "HSD",
		"quantity_liters": "12.500",
		"unit_price":      "275.50",
		"total_amount":    "3443.75",
		"payment_method":  "CASH",
		"customer_phone":  "",
		"pump_id":         "P1",
		"attendant":       "Ali",
	})
	processed, errs, err := n.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if errs != 0 {
		t.Errorf("errors = %d, want 0", errs)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}

	// Re-run: should be a no-op (idempotent).
	processed, errs, err = n.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 || errs != 0 {
		t.Errorf("re-run: processed=%d errors=%d, want 0/0", processed, errs)
	}
	_ = id
}

func TestNormalizeRowRejectsUnknownAlias(t *testing.T) {
	n := newTestNormalizer(t)
	rawFromRow(t, n.store, "b1", "lane_1", map[string]string{
		"pos_source_id":   "lane_1",
		"external_id":     "TX-002",
		"occurred_at":     "2026-09-07T08:00:00Z",
		"product_alias":   "GASOLINE_98",
		"quantity_liters": "10",
		"unit_price":      "300",
		"total_amount":    "3000",
		"payment_method":  "CASH",
	})
	processed, errs, err := n.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0 (unknown alias)", processed)
	}
	if errs != 1 {
		t.Errorf("errors = %d, want 1", errs)
	}
}

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"12.5", 12.5, true},
		{"12.500", 12.5, true},
		{"275.50", 275.5, true},
		{"1,234.56", 1234.56, true},
		{"1,234,567.89", 1234567.89, true},
		{"  1,000  ", 1000, true},
		{"0", 0, true},
		{"-50.25", -50.25, true},
		{"", 0, false},
		{"abc", 0, false},
		{"12.34.56", 0, false},
	}
	for _, c := range cases {
		got, err := parseDecimal(c.in)
		if (err == nil) != c.ok {
			t.Errorf("parseDecimal(%q) ok=%v, want ok=%v (err=%v)", c.in, err == nil, c.ok, err)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseDecimal(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
