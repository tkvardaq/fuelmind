package posadapter

import "time"

// PayloadKind is a hint for the normalizer (Phase 2) so it can pick the
// right parser without sniffing bytes. Values: "json", "csv", "xml", "bin".
type PayloadKind string

const (
	PayloadJSON PayloadKind = "json"
	PayloadCSV  PayloadKind = "csv"
	PayloadXML  PayloadKind = "xml"
	PayloadBin  PayloadKind = "bin"
)

// RawTransaction is the canonical "raw" shape that flows into
// raw_pos_transactions. The Payload is the original, untouched bytes —
// we never parse/normalize at this layer (see spec §2.3).
type RawTransaction struct {
	PosSourceID string       // which POS / lane it came from (e.g. "lane_1")
	ExternalID  string       // the POS's own ID for this transaction, if any
	OccurredAt  time.Time    // when the POS says the transaction happened
	Payload     []byte       // original JSON/CSV row, untouched
	PayloadKind PayloadKind  // hint for the normalizer
}

// RawInventory is the raw inventory snapshot from a tank gauge.
type RawInventory struct {
	TankSourceID string
	OccurredAt   time.Time
	Payload      []byte
	PayloadKind  PayloadKind
}

// RawShift is the raw shift / attendant data.
type RawShift struct {
	ShiftSourceID string
	OccurredAt    time.Time
	Payload       []byte
	PayloadKind   PayloadKind
}
