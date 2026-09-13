// Command gendata writes realistic POS exports for testing a FuelMind
// install: several days of sales across three pumps and three products,
// with the cash/card/credit mix a real station sees.
//
//	go run ./tools/gendata -out C:\ProgramData\FuelMind\pos_drop -days 7
//
// The numbers are generated from a fixed seed, so the same flags always
// produce the same files and a figure on the dashboard can be checked by
// hand against the CSV that produced it.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type product struct {
	alias string // what the POS calls it
	code  string
	price float64
}

// The aliases are deliberately the messy ones a real POS emits; the
// normalizer is supposed to map them.
var products = []product{
	{"HSD", "DIESEL", 275.50},
	{"Hi-Speed Diesel", "DIESEL", 275.50},
	{"PMG-92", "PETROL_92", 250.30},
	{"PETROL_92", "PETROL_92", 250.30},
	{"PETROL_95", "PETROL_95", 278.40},
}

var (
	attendants = []string{"Ali", "Bilal", "Choudhry", "Danish"}
	lanes      = []string{"lane_1", "lane_2", "lane_3"}
	pumps      = []string{"P1", "P2", "P3", "P4"}
	// Regulars who buy on credit. The last one stops buying part-way
	// through the week, which is what a station wants to be told about.
	creditCustomers = []string{"+923001234567", "+923009876543", "+923331112222"}
)

func main() {
	out := flag.String("out", "testdata/generated", "folder to write the CSV files into")
	days := flag.Int("days", 7, "how many days to generate, ending today")
	perDay := flag.Int("per-day", 55, "roughly how many sales per day")
	seed := flag.Int64("seed", 20260913, "random seed; same seed, same data")
	bad := flag.Bool("bad", false, "also write a file with malformed rows, to test error handling")
	xlsx := flag.Bool("xlsx", true, "also write an .xlsx copy of the newest day for opening in Excel")
	flag.Parse()

	if err := run(*out, *days, *perDay, *seed, *bad, *xlsx); err != nil {
		fmt.Fprintln(os.Stderr, "gendata:", err)
		os.Exit(1)
	}
}

func run(out string, days, perDay int, seed int64, bad, xlsx bool) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(seed))
	// Midnight local, not time.Truncate: truncation is relative to the
	// UTC epoch, which in any non-UTC zone tips the last sales of a day
	// into the next one — and then the dashboard is right while the file
	// name is wrong.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	var newest [][]string
	for d := days - 1; d >= 0; d-- {
		day := today.AddDate(0, 0, -d)
		rows := generateDay(rng, day, perDay, d)
		name := filepath.Join(out, fmt.Sprintf("transactions_%s.csv", day.Format("20060102")))
		if err := writeCSV(name, rows); err != nil {
			return err
		}
		fmt.Printf("%s  %d sales\n", name, len(rows))
		newest = rows
	}

	// One older credit sale nobody has paid off: this is what makes the
	// credit alert fire, and it belongs to a customer who then stopped
	// coming.
	stale := today.AddDate(0, 0, -41)
	staleRows := [][]string{transaction(rng, stale.Add(11*time.Hour), "TX-STALE-0001", products[0], 60, "CREDIT", creditCustomers[2])}
	staleName := filepath.Join(out, fmt.Sprintf("transactions_%s.csv", stale.Format("20060102")))
	if err := writeCSV(staleName, staleRows); err != nil {
		return err
	}
	fmt.Printf("%s  1 sale (old unpaid credit)\n", staleName)

	if bad {
		name := filepath.Join(out, "transactions_malformed.csv")
		if err := os.WriteFile(name, []byte(malformed), 0o644); err != nil {
			return err
		}
		fmt.Printf("%s  deliberately broken, should land in failed/\n", name)
	}
	if xlsx && len(newest) > 0 {
		name := filepath.Join(out, "transactions_today.xlsx")
		if err := writeXLSX(name, append([][]string{header}, newest...)); err != nil {
			return err
		}
		fmt.Printf("%s  same rows, for opening in Excel\n", name)
	}
	return nil
}

var header = []string{
	"pos_source_id", "external_id", "occurred_at", "product_alias",
	"quantity_liters", "unit_price", "total_amount", "payment_method",
	"customer_phone", "pump_id", "attendant",
}

// generateDay builds one day of sales. Volume rises through the morning
// and evening rush, the way a station on a main road actually sells.
func generateDay(rng *rand.Rand, day time.Time, perDay, daysAgo int) [][]string {
	n := perDay + rng.Intn(15) - 7
	rows := make([][]string, 0, n)
	for i := 0; i < n; i++ {
		at := day.Add(time.Duration(rushHour(rng)) * time.Hour).Add(time.Duration(rng.Intn(60)) * time.Minute)
		p := products[rng.Intn(len(products))]
		liters := float64(rng.Intn(45)+5) + float64(rng.Intn(100))/100
		method, phone := payment(rng, daysAgo)
		id := fmt.Sprintf("TX-%s-%04d", day.Format("20060102"), i+1)
		rows = append(rows, transaction(rng, at, id, p, liters, method, phone))
	}
	return rows
}

// rushHour returns an hour of the day weighted towards the two rushes.
func rushHour(rng *rand.Rand) int {
	switch n := rng.Intn(10); {
	case n < 3:
		return 7 + rng.Intn(3) // morning
	case n < 7:
		return 17 + rng.Intn(4) // evening
	default:
		return 10 + rng.Intn(7)
	}
}

// payment picks how a sale was paid for. The third credit customer stops
// buying three days in, so their balance goes quiet.
func payment(rng *rand.Rand, daysAgo int) (method, phone string) {
	switch n := rng.Intn(20); {
	case n < 11:
		return "CASH", ""
	case n < 15:
		return "CARD", ""
	case n < 17:
		return "MOBILE_WALLET", ""
	default:
		customers := creditCustomers
		if daysAgo < 3 {
			customers = creditCustomers[:2] // the third has stopped coming
		}
		return "CREDIT", customers[rng.Intn(len(customers))]
	}
}

func transaction(rng *rand.Rand, at time.Time, id string, p product, liters float64, method, phone string) []string {
	total := round2(liters * p.price)
	return []string{
		lanes[rng.Intn(len(lanes))],
		id,
		at.Format("2006-01-02T15:04:05"),
		p.alias,
		strconv.FormatFloat(liters, 'f', 3, 64),
		strconv.FormatFloat(p.price, 'f', 2, 64),
		strconv.FormatFloat(total, 'f', 2, 64),
		method,
		phone,
		pumps[rng.Intn(len(pumps))],
		attendants[rng.Intn(len(attendants))],
	}
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func writeCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// malformed exercises the per-row error path: a short row, a
// non-numeric quantity, and a date the parser cannot read.
const malformed = `pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method,customer_phone,pump_id,attendant
lane_1,TX-BAD-0001,2026-09-13T08:00:00,HSD,not-a-number,275.50,3443.75,CASH,,P1,Ali
lane_1,TX-BAD-0002,yesterday morning,HSD,12.500,275.50,3443.75,CASH,,P1,Ali
lane_1,TX-BAD-0003,2026-09-13T08:00:00,HSD
`
