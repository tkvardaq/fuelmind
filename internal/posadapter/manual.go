package posadapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ManualSourceID is the pos_source_id given to sales typed in by hand.
// It is deliberately visible in the data: an owner looking at a lane
// breakdown should be able to tell which figures came off the POS and
// which were entered by a person.
const ManualSourceID = "MANUAL"

// ManualSale is one sale entered by hand, for when the POS is down, the
// export is late, or a pump was written up in the day book. The fields
// are the ones a person can actually know standing at the counter.
type ManualSale struct {
	OccurredAt     time.Time
	ProductAlias   string // matched against product_aliases, same as a POS row
	QuantityLiters float64
	UnitPrice      float64
	TotalAmount    float64 // 0 means "work it out from quantity × price"
	PaymentMethod  string
	CustomerPhone  string // required for CREDIT, so the sale lands on an account
	PumpID         string
	Attendant      string
	Note           string
	EnteredBy      string
}

// ManualSaleError is a problem with what was typed in, worded for the
// person who typed it rather than for a log.
type ManualSaleError struct {
	Field   string
	Message string
}

func (e ManualSaleError) Error() string { return e.Message }

// BuildManualRow validates a hand-entered sale and turns it into the
// same RawTransaction a CSV row produces, so it flows through exactly
// the same normalization, product-alias resolution, data-quality
// flagging and mart materialization. Nothing about a manual sale gets a
// shortcut — that is the point.
func BuildManualRow(s ManualSale) (RawTransaction, error) {
	if s.OccurredAt.IsZero() {
		return RawTransaction{}, ManualSaleError{"occurred_at", "Choose the date and time of the sale."}
	}
	if s.OccurredAt.After(time.Now().Add(2 * time.Minute)) {
		return RawTransaction{}, ManualSaleError{"occurred_at", "That time is in the future. Check the date and time."}
	}
	if strings.TrimSpace(s.ProductAlias) == "" {
		return RawTransaction{}, ManualSaleError{"product_alias", "Choose which fuel was sold."}
	}
	if s.QuantityLiters <= 0 {
		return RawTransaction{}, ManualSaleError{"quantity_liters", "Enter how many litres were sold, for example 12.5."}
	}
	if s.UnitPrice <= 0 {
		return RawTransaction{}, ManualSaleError{"unit_price", "Enter the price per litre, for example 275.50."}
	}

	total := s.TotalAmount
	if total <= 0 {
		total = s.QuantityLiters * s.UnitPrice
	}

	method := strings.ToUpper(strings.TrimSpace(s.PaymentMethod))
	if method == "" {
		method = "CASH"
	}
	if method == "CREDIT" && strings.TrimSpace(s.CustomerPhone) == "" {
		return RawTransaction{}, ManualSaleError{
			"customer_phone",
			"A credit sale needs the customer's phone number, otherwise there is no account to put it on.",
		}
	}

	// An id that is stable for the same sale and unique across different
	// ones. The raw layer rejects a duplicate payload hash, so entering
	// the same sale twice by accident is a no-op rather than double
	// revenue — but two genuinely different sales in the same second on
	// the same pump still both count, because the amounts differ.
	external := fmt.Sprintf("MANUAL-%s-%s-%.3f-%.2f",
		s.OccurredAt.Format("20060102T150405"),
		strings.ToUpper(strings.TrimSpace(s.ProductAlias)),
		s.QuantityLiters, total)

	row := map[string]string{
		"pos_source_id":   ManualSourceID,
		"external_id":     external,
		"occurred_at":     s.OccurredAt.Format(time.RFC3339),
		"product_alias":   strings.TrimSpace(s.ProductAlias),
		"quantity_liters": fmt.Sprintf("%.3f", s.QuantityLiters),
		"unit_price":      fmt.Sprintf("%.2f", s.UnitPrice),
		"total_amount":    fmt.Sprintf("%.2f", total),
		"payment_method":  method,
		"customer_phone":  strings.TrimSpace(s.CustomerPhone),
		"pump_id":         strings.TrimSpace(s.PumpID),
		"attendant":       strings.TrimSpace(s.Attendant),
		"entered_by":      strings.TrimSpace(s.EnteredBy),
		"entry_note":      strings.TrimSpace(s.Note),
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return RawTransaction{}, err
	}
	return RawTransaction{
		PosSourceID: ManualSourceID,
		ExternalID:  external,
		OccurredAt:  s.OccurredAt,
		Payload:     payload,
		PayloadKind: PayloadJSON,
	}, nil
}
