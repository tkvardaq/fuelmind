package csvwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/posadapter"
)

func TestParseFileOK(t *testing.T) {
	in := `pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method
lane_1,TX-001,2026-09-07T08:00:00Z,HSD,12.5,275.50,3437.50,CASH
lane_2,TX-002,2026-09-07T08:01:00Z,PETROL_92,10.0,250.30,2503.00,CARD
`
	rows, errs, err := parseFile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected parse errors: %v", errs)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	if rows[0].PosSourceID != "lane_1" || rows[0].ExternalID != "TX-001" {
		t.Errorf("row 0 wrong: %+v", rows[0])
	}
	want := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	if !rows[0].OccurredAt.Equal(want) {
		t.Errorf("row 0 time = %v, want %v", rows[0].OccurredAt, want)
	}
	if rows[0].PayloadKind != posadapter.PayloadJSON {
		t.Errorf("row 0 payload kind = %q, want json", rows[0].PayloadKind)
	}
	// Payload should contain the column data.
	if !strings.Contains(string(rows[0].Payload), `"product_alias":"HSD"`) {
		t.Errorf("row 0 payload missing product_alias: %s", rows[0].Payload)
	}
}

func TestParseFileMissingColumn(t *testing.T) {
	in := `pos_source_id,external_id,occurred_at
lane_1,TX-001,2026-09-07T08:00:00Z
`
	_, _, err := parseFile(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected error for missing required column")
	}
	if !strings.Contains(err.Error(), "missing required column") {
		t.Errorf("expected missing-column error, got: %v", err)
	}
}

func TestParseFileBadTimestamp(t *testing.T) {
	in := `pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method
lane_1,TX-001,not-a-timestamp,HSD,12.5,275.50,3437.50,CASH
`
	rows, errs, err := parseFile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
	if len(errs) != 1 {
		t.Errorf("expected 1 parse error, got %d: %v", len(errs), errs)
	}
}

func TestParseFileMixedAliases(t *testing.T) {
	// Mixed vendor aliases — HSD, Hi-Speed Diesel, DIESEL, Diesel all
	// resolve to the same canonical product (Phase 2 handles that). The
	// parser doesn't care; it just emits the raw alias in the payload.
	in := `pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method
lane_1,T1,2026-09-07T08:00:00Z,HSD,1,1,1,CASH
lane_1,T2,2026-09-07T08:01:00Z,Hi-Speed Diesel,1,1,1,CASH
lane_1,T3,2026-09-07T08:02:00Z,DIESEL,1,1,1,CASH
lane_1,T4,2026-09-07T08:03:00Z,Diesel,1,1,1,CASH
`
	rows, _, err := parseFile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(rows) != 4 {
		t.Errorf("expected 4 rows, got %d", len(rows))
	}
	aliases := []string{"HSD", "Hi-Speed Diesel", "DIESEL", "Diesel"}
	for i, r := range rows {
		if !strings.Contains(string(r.Payload), `"product_alias":"`+aliases[i]+`"`) {
			t.Errorf("row %d payload missing alias %q: %s", i, aliases[i], r.Payload)
		}
	}
}
