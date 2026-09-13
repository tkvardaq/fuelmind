package storage

import (
	"context"
	"fmt"

	"github.com/fuelmind/fuelmind/internal/phone"
)

// ConfigKeyPhonesCanonical marks that the one-time phone backfill has run.
const ConfigKeyPhonesCanonical = "customer_phones_canonicalized"

// CanonicalizeCustomerPhones rewrites every stored customer phone into
// the canonical form, so a station upgrading from an earlier build stops
// showing one customer as several credit accounts.
//
// It runs the same Go function the normalizer uses rather than trying to
// express the rule in SQL, so the two can never drift. It reports how
// many rows it changed; zero means everything was already canonical.
//
// The work is idempotent — running it twice changes nothing the second
// time — but the caller guards it with ConfigKeyPhonesCanonical so a
// station with years of history does not pay for the scan on every boot.
func (s *Storage) CanonicalizeCustomerPhones(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	changed := 0
	for _, table := range []string{"transactions", "credit_payments"} {
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT customer_phone FROM `+table+
				` WHERE customer_phone IS NOT NULL AND customer_phone <> ''`)
		if err != nil {
			return 0, fmt.Errorf("storage: read phones from %s: %w", table, err)
		}
		rewrites := map[string]string{}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return 0, err
			}
			if canonical := phone.Normalize(raw); canonical != raw {
				rewrites[raw] = canonical
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		rows.Close()

		for raw, canonical := range rewrites {
			res, err := tx.ExecContext(ctx,
				`UPDATE `+table+` SET customer_phone = ? WHERE customer_phone = ?`,
				canonical, raw)
			if err != nil {
				return 0, fmt.Errorf("storage: rewrite phone in %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			changed += int(n)
		}
	}

	// credit_outstanding is a mart table keyed by the old phone values.
	// It is fully derived, so clearing it is safe: the caller rebuilds
	// the mart, which writes correct rows under the canonical keys.
	if changed > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM credit_outstanding`); err != nil {
			return 0, fmt.Errorf("storage: clear credit_outstanding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}
