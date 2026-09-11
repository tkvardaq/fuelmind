# POS sample fixtures

These CSV files define the v1 wire format that every FuelMind POS adapter
must produce (or be able to read). The v1 default adapter, `csv_watch`,
expects files in exactly this format. Future vendor-specific adapters
(vendor X DB poll, vendor Y serial, etc.) will be tested against the same
fixtures via the contract test harness.

## File location

A FuelMind install points its `csv_watch` adapter at a folder (e.g.
`C:\ProgramData\FuelMind\pos_drop`). Any POS system that can export to
CSV drops files there. After successful ingestion the file is moved to
`pos_drop\processed\`; on parse error, to `pos_drop\failed\`.

## Required columns (header row, comma-separated)

| Column | Type | Example | Notes |
|---|---|---|---|
| `pos_source_id` | string | `lane_1` | Which POS / lane / register the row came from |
| `external_id` | string | `TX-20260907-0001` | The POS's own transaction ID — used by the normalizer for idempotency |
| `occurred_at` | RFC 3339 timestamp | `2026-09-07T08:14:22` | When the POS says the transaction happened (NOT when the file was dropped) |
| `product_alias` | string | `HSD` | Vendor-specific name; resolved to canonical `DIESEL` / `PETROL_92` / `PETROL_95` in the normalizer (Phase 2) |
| `quantity_liters` | decimal | `12.500` | Volume, 3 decimal places |
| `unit_price` | decimal | `275.50` | Price per liter, 2 decimal places, in the station's local currency (PKR for PK) |
| `total_amount` | decimal | `3443.75` | Total charged; the normalizer recomputes this for audit |
| `payment_method` | enum | `CASH` / `CARD` / `CREDIT` / `MOBILE_WALLET` | Free text but expected to be one of these values |
| `customer_phone` | string (optional) | `+923001234567` | E.164 with leading `+`; only present for credit / mobile wallet |
| `pump_id` | string (optional) | `P1` | Pump / dispenser identifier |
| `attendant` | string (optional) | `Ali` | Attendant name on shift |

## Why aliases in the wire format

Different POS vendors call the same fuel by different names:

- `HSD` = `Hi-Speed Diesel` = `Diesel` = `DIESEL` → all canonicalize to `DIESEL`
- `PMG-92` = `PETROL_92` = `P-92` = `Petrol` → all canonicalize to `PETROL_92`
- `PMG-95` = `PETROL_95` = `P-95` → all canonicalize to `PETROL_95`

The Phase 1 fixture files deliberately mix aliases across rows to make
sure the normalizer's alias table covers real vendor variations seen in
the Pakistan / regional market.

## What's in the fixtures

- `transactions_sample_01.csv` — 10 rows, all four payment methods, mixed
  product aliases (HSD, Hi-Speed Diesel, PETROL_92, P-92, DIESEL, PETROL_95,
  Diesel, PMG-92)
- `transactions_sample_02.csv` — 5 rows, mostly afternoon fuel, two large
  credit sales (the kind that drive `credit_outstanding` in the data mart)

## Adding your own fixtures

When a real station's POS exports a file, sanitize it and drop it here
**after** removing or pseudonymizing any PII (customer names, real phone
numbers, exact card last-4, etc.). The fixture corpus is the most
durable test asset in the project — invest in keeping it realistic.
