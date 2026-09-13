// Package normalizer turns raw_pos_transactions rows into
// `transactions` rows. This is where `HSD` / `Hi-Speed Diesel` /
// `DIESEL` / `Diesel` collapse to canonical `DIESEL`, and where the
// CSV's string columns become typed numbers.
//
// Per spec §2.3, this is Layer 2 of the three-layer data model.
//
// Idempotency: a row in `transactions` UNIQUEly references a
// `raw_transaction_id`, and (pos_source_id, external_id) is unique, so
// re-running on the same raw rows — or on a re-export of the same POS
// transactions — never double-counts.
package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/phone"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// knownPaymentMethods we accept. Anything else is preserved (upper-cased,
// trimmed) and a warning is logged — we want the owner to see the data.
var knownPaymentMethods = map[string]struct{}{
	"CASH":          {},
	"CARD":          {},
	"CREDIT":        {},
	"MOBILE_WALLET": {},
}

// Data-quality flags stored on accepted rows.
const (
	FlagTotalMismatch    = "total_mismatch"
	FlagNegativeQuantity = "negative_quantity"
)

// Normalizer is the conversion pipeline.
type Normalizer struct {
	store   *storage.Storage
	aliases map[string]string
	logger  *slog.Logger
	loc     *time.Location
}

// New builds a normalizer. Timestamps are converted to the station's
// local time zone (time.Local) so the mart buckets sales by the
// station's business day regardless of how the POS writes offsets.
func New(store *storage.Storage, logger *slog.Logger) (*Normalizer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	n := &Normalizer{store: store, logger: logger, loc: time.Local}
	if err := n.reloadAliases(context.Background()); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Normalizer) reloadAliases(ctx context.Context) error {
	aliases, err := n.store.ProductAliases(ctx)
	if err != nil {
		return fmt.Errorf("normalizer: load aliases: %w", err)
	}
	lower := make(map[string]string, len(aliases)*2)
	for k, v := range aliases {
		lower[k] = v
		lower[strings.ToLower(k)] = v
	}
	n.aliases = lower
	return nil
}

// Run processes up to `limit` un-normalized rows (0 = no limit). Rows that
// fail (unknown alias, bad decimal, …) are recorded in
// raw_normalize_errors and retried on the next run, so adding a missing
// alias heals them. Each failure is logged once.
func (n *Normalizer) Run(ctx context.Context, limit int) (processed, errs int, err error) {
	// Aliases are cheap to reload and this lets a newly added alias take
	// effect without restarting the service.
	if err := n.reloadAliases(ctx); err != nil {
		return 0, 0, err
	}
	rows, err := n.store.UnnormalizedTransactions(ctx, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("normalizer: query: %w", err)
	}
	for _, r := range rows {
		nrow, nerr := n.normalizeRow(r)
		if nerr != nil {
			errs++
			first, rerr := n.store.RecordNormalizeError(ctx, r.ID, nerr.Error())
			if rerr != nil {
				return processed, errs, rerr
			}
			if first {
				n.logger.Warn("normalize row failed", "raw_id", r.ID, "err", nerr)
			}
			continue
		}
		replaced, werr := n.store.WriteNormalizedTransaction(ctx, nrow)
		if werr != nil {
			return processed, errs, fmt.Errorf("normalizer: write: %w", werr)
		}
		if replaced {
			n.logger.Info("POS re-exported a transaction; replaced the earlier copy",
				"pos_source_id", nrow.PosSourceID, "external_id", nrow.ExternalID)
		}
		processed++
	}
	return processed, errs, nil
}

// csvRow is the on-the-wire shape csvwatch produced.
type csvRow map[string]string

func (n *Normalizer) normalizeRow(r storage.RawTransaction) (storage.NormalizedTransaction, error) {
	var row csvRow
	if err := json.Unmarshal([]byte(r.RawPayload), &row); err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("payload is not JSON: %w", err)
	}

	// 1. Resolve product alias (exact, then case-insensitive).
	alias := strings.TrimSpace(row["product_alias"])
	code, ok := n.aliases[alias]
	if !ok {
		code, ok = n.aliases[strings.ToLower(alias)]
	}
	if !ok {
		return storage.NormalizedTransaction{}, fmt.Errorf("unknown product %q (add it to product_aliases)", alias)
	}

	// 2. Parse decimals.
	qty, err := parseDecimal(row["quantity_liters"])
	if err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("quantity_liters: %w", err)
	}
	price, err := parseDecimal(row["unit_price"])
	if err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("unit_price: %w", err)
	}
	total, err := parseDecimal(row["total_amount"])
	if err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("total_amount: %w", err)
	}

	// 3. Data-quality flags. The POS total is kept as the revenue figure
	//    (it is what the customer paid) but inconsistencies are flagged.
	var flags []string
	if !amountsAgree(qty, price, total) {
		flags = append(flags, FlagTotalMismatch)
	}
	if qty < 0 {
		flags = append(flags, FlagNegativeQuantity)
	}

	// 4. Payment method.
	pay := strings.ToUpper(strings.TrimSpace(row["payment_method"]))
	if _, known := knownPaymentMethods[pay]; !known {
		n.logger.Warn("unknown payment_method, preserving as-is", "raw_id", r.ID, "value", pay)
	}

	// 5. Timestamp → station-local RFC 3339.
	t, err := ParseTimestamp(strings.TrimSpace(row["occurred_at"]), n.loc)
	if err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("occurred_at: %w", err)
	}

	return storage.NormalizedTransaction{
		RawTransactionID: r.ID,
		PosSourceID:      r.PosSourceID,
		ExternalID:       strings.TrimSpace(row["external_id"]),
		ProductCode:      code,
		QuantityLiters:   qty,
		UnitPrice:        price,
		TotalAmount:      total,
		PaymentMethod:    pay,
		CustomerPhone:    phone.Normalize(row["customer_phone"]),
		PumpID:           strings.TrimSpace(row["pump_id"]),
		Attendant:        strings.TrimSpace(row["attendant"]),
		TransactionTime:  t.In(n.loc).Format(time.RFC3339),
		Flags:            strings.Join(flags, ","),
	}, nil
}

// amountsAgree reports whether total ≈ quantity × price, allowing for
// POS rounding (1 currency unit or 0.5%, whichever is larger).
func amountsAgree(qty, price, total float64) bool {
	want := qty * price
	tol := math.Max(1.0, math.Abs(total)*0.005)
	return math.Abs(want-total) <= tol
}

// ParseTimestamp accepts RFC 3339 (with zone) and the naive forms
// "2006-01-02T15:04:05" / "2006-01-02 15:04:05", which are interpreted
// in loc (the station's time zone).
func ParseTimestamp(s string, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable %q", s)
}

// parseDecimal accepts "12.5", "1,234.56" (thousands separators).
func parseDecimal(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty decimal string")
	}
	s = strings.ReplaceAll(s, ",", "")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal %q: %w", s, err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, fmt.Errorf("decimal %q is NaN or Inf", s)
	}
	return val, nil
}
