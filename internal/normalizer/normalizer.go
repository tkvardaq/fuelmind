// Package normalizer turns raw_pos_transactions rows into
// `transactions` rows. This is where `HSD` / `Hi-Speed Diesel` /
// `DIESEL` / `Diesel` collapse to canonical `DIESEL`, and where the
// CSV's string columns become typed numbers.
//
// Per spec §2.3, this is Layer 2 of the three-layer data model.
// Everything above the mart is read from the result of this
// normalizer; nothing in the LLM or dashboard ever sees raw POS
// quirks.
//
// The normalizer is intentionally simple in v1: a literal-string
// alias lookup. Fuzzy / embedding-based alias matching is v1.1.
//
// Idempotency: a row in `transactions` UNIQUEly references a
// `raw_transaction_id`, so re-running on the same raw rows is a
// no-op. The Watcher calls this after every ingestion batch.
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

	"github.com/fuelmind/fuelmind/internal/storage"
)

// paymentMethods we accept. Anything else is preserved as-is (lowercased,
// trimmed) and a warning is logged — we don't reject the row because
// we want the owner to see the data, not lose it.
var knownPaymentMethods = map[string]struct{}{
	"CASH":          {},
	"CARD":          {},
	"CREDIT":        {},
	"MOBILE_WALLET": {},
}

// Normalizer is the conversion pipeline. Holds an in-memory alias map
// and a logger. One per Storage.
type Normalizer struct {
	store   *storage.Storage
	aliases map[string]string
	logger  *slog.Logger
}

// New builds a normalizer and eagerly loads the alias map. If the
// alias table is empty the normalizer still works — every alias
// lookup will fail and the row will be marked unresolvable.
func New(store *storage.Storage, logger *slog.Logger) (*Normalizer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	aliases, err := store.ProductAliases(context.Background())
	if err != nil {
		return nil, fmt.Errorf("normalizer: load aliases: %w", err)
	}
	return &Normalizer{store: store, aliases: aliases, logger: logger}, nil
}

// Run processes up to `limit` un-normalized rows (0 = no limit). It
// is safe to call repeatedly; each call processes new rows only
// (the WHERE t.id IS NULL clause excludes already-normalized ones).
//
// Returns the number of rows successfully written. Rows that fail
// (bad alias, bad decimal, etc.) are counted as errors and reported
// in the log; they are NOT written, so the next call retries them.
func (n *Normalizer) Run(ctx context.Context, limit int) (processed, errors int, err error) {
	rows, err := n.store.UnnormalizedTransactions(ctx, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("normalizer: query: %w", err)
	}
	for _, r := range rows {
		nrow, nerr := n.normalizeRow(r)
		if nerr != nil {
			n.logger.Warn("normalize row failed",
				"raw_id", r.ID, "err", nerr)
			errors++
			continue
		}
		if werr := n.store.WriteNormalizedTransaction(ctx, nrow); werr != nil {
			return processed, errors, fmt.Errorf("normalizer: write: %w", werr)
		}
		processed++
	}
	return processed, errors, nil
}

// csvRow is the on-the-wire shape csvwatch produced. The normalizer
// is the only consumer of this struct; if csvwatch changes, the
// normalizer must change in lockstep.
type csvRow map[string]string

// normalizeRow is the per-row work. It is exported (within the
// package) for testing.
func (n *Normalizer) normalizeRow(r storage.RawTransaction) (storage.NormalizedTransaction, error) {
	var row csvRow
	if err := json.Unmarshal([]byte(r.RawPayload), &row); err != nil {
		return storage.NormalizedTransaction{}, fmt.Errorf("payload is not JSON: %w", err)
	}

	// 1. Resolve product alias.
	alias := strings.TrimSpace(row["product_alias"])
	code, ok := n.aliases[alias]
	if !ok {
		// Try case-insensitive fallback. Most POSes write in
		// uppercase, but Excel exports can shift case.
		for k, v := range n.aliases {
			if strings.EqualFold(k, alias) {
				code = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return storage.NormalizedTransaction{}, fmt.Errorf("unresolved product alias %q (add a row to product_aliases)", alias)
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

	// 3. Validate payment method.
	pay := strings.ToUpper(strings.TrimSpace(row["payment_method"]))
	if _, known := knownPaymentMethods[pay]; !known {
		n.logger.Warn("unknown payment_method, preserving as-is",
			"raw_id", r.ID, "value", pay)
	}

	// 4. Parse the transaction timestamp. The raw payload's
	//    occurred_at is what the POS said, not received_at
	//    (the file-drop time).
	txTime := strings.TrimSpace(row["occurred_at"])
	if _, err := time.Parse(time.RFC3339, txTime); err == nil {
		// Good: full RFC 3339. Keep as-is.
	} else if t, err := time.ParseInLocation("2006-01-02T15:04:05", txTime, time.Local); err == nil {
		txTime = t.Format(time.RFC3339)
	} else if t, err := time.ParseInLocation("2006-01-02 15:04:05", txTime, time.Local); err == nil {
		txTime = t.Format(time.RFC3339)
	} else {
		return storage.NormalizedTransaction{}, fmt.Errorf("occurred_at: unparseable %q", txTime)
	}

	return storage.NormalizedTransaction{
		RawTransactionID: r.ID,
		ProductCode:      code,
		QuantityLiters:   qty,
		UnitPrice:        price,
		TotalAmount:      total,
		PaymentMethod:    pay,
		CustomerPhone:    strings.TrimSpace(row["customer_phone"]),
		PumpID:           strings.TrimSpace(row["pump_id"]),
		Attendant:        strings.TrimSpace(row["attendant"]),
		TransactionTime:  txTime,
	}, nil
}

// parseDecimal accepts "12.5", "12.500", "1,234.56", and "1,234,567.89"
// (thousands comma separators). Regional POSes in PK often emit
// comma-separated values, and "1,234.56 PKR" is a common Excel export.
func parseDecimal(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty decimal string")
	}
	// Strip thousands-separator commas
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
