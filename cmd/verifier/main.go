package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Result is what the verifier reports. Also written to JSON for HTML report consumption.
type Result struct {
	LedgerSum            int64    `json:"ledger_sum"`             // I1: must = 0
	DriftedAccounts      []string `json:"drifted_accounts"`       // I2: balance != initial + SUM(ledger)
	NegativeBalanceAccts []string `json:"negative_balance_accts"` // app invariant: balance >= 0
	OrphanTxnIDs         []string `json:"orphan_txn_ids"`         // I3: txn_id without exactly 2 rows
	Pass                 bool     `json:"pass"`
}

// Verifier checks 3 invariants + the app-level "no negative balance" rule.
//
// TODO (you): implement.
//
// Step 1 — parse flags:
//   --db <dsn>          (defaults to env DB_URL)
//   --initial-balance N (default 100000 — must match what 002_seed.sql inserted)
//
// Step 2 — open DB connection with sql.Open("pgx", dsn). Defer Close.
//
// Step 3 — run these 4 queries and populate Result:
//
//   I1 — ledger sum:
//     SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries
//     Scan into res.LedgerSum. If != 0 → res.Pass = false.
//
//   I2 — balance drift:
//     SELECT a.id
//     FROM accounts a
//     LEFT JOIN ledger_entries l ON l.account_id = a.id
//     GROUP BY a.id, a.balance_minor
//     HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + $1
//     (with $1 = initial-balance)
//     Append each returned id to res.DriftedAccounts. If any → res.Pass = false.
//
//   Negative balances:
//     SELECT id FROM accounts WHERE balance_minor < 0
//     Append to res.NegativeBalanceAccts. If any → res.Pass = false.
//
//   I3 — orphan txn_ids (txn_id without exactly 2 rows):
//     SELECT txn_id::text FROM ledger_entries
//     GROUP BY txn_id HAVING COUNT(*) != 2
//     Append to res.OrphanTxnIDs. If any → res.Pass = false.
//
// Step 4 — pretty-print to stdout:
//   Use printCheck helper (below). Format:
//     === Verifier Report ===
//       ✓ I1 ledger sum == 0           sum=0
//       ✓ I2 no balance drift           drifted=[]
//       ✓     no negative balances      negative=[]
//       ✓ I3 all txns have 2 entries    orphans=[]
//
// Step 5 — emit JSON after the human-readable report (for HTML reports):
//   "\n--- JSON ---\n"
//   <json.MarshalIndent of res>
//
// Step 6 — exit code:
//   os.Exit(1) if !res.Pass, else 0.
//
// Hint: rows.Next() loop pattern in Go:
//   rows, err := dbx.Query(`...`, args...)
//   if err != nil { ... }
//   defer rows.Close()
//   for rows.Next() {
//       var id string
//       if err := rows.Scan(&id); err != nil { ... }
//       res.DriftedAccounts = append(res.DriftedAccounts, id)
//   }
func main() {
	dbURL := flag.String("db", os.Getenv("DB_URL"), "postgres DSN")
	initialBalance := flag.Int64("initial-balance", 100000, "seeded balance per account")
	flag.Parse()

	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "--db or DB_URL required")
		os.Exit(2)
	}

	dbx, err := sql.Open("pgx", *dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(2)
	}
	defer dbx.Close()

	res := Result{
		DriftedAccounts:      []string{},
		NegativeBalanceAccts: []string{},
		OrphanTxnIDs:         []string{},
		Pass:                 true,
	}

	// I1 — ledger sum
	err = dbx.QueryRow("SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries").Scan(&res.LedgerSum)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query I1 failed: %v\n", err)
		os.Exit(2)
	}
	if res.LedgerSum != 0 {
		res.Pass = false
	}

	// I2 — balance drift
	rowsI2, err := dbx.Query(`
		SELECT a.id
		FROM accounts a
		LEFT JOIN ledger_entries l ON l.account_id = a.id
		GROUP BY a.id, a.balance_minor
		HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + $1
	`, *initialBalance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query I2 failed: %v\n", err)
		os.Exit(2)
	}
	defer rowsI2.Close()
	for rowsI2.Next() {
		var id string
		if err := rowsI2.Scan(&id); err != nil {
			fmt.Fprintf(os.Stderr, "scan I2 failed: %v\n", err)
			os.Exit(2)
		}
		res.DriftedAccounts = append(res.DriftedAccounts, id)
	}
	if len(res.DriftedAccounts) > 0 {
		res.Pass = false
	}

	// Negative balances
	rowsNeg, err := dbx.Query("SELECT id FROM accounts WHERE balance_minor < 0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query negative balances failed: %v\n", err)
		os.Exit(2)
	}
	defer rowsNeg.Close()
	for rowsNeg.Next() {
		var id string
		if err := rowsNeg.Scan(&id); err != nil {
			fmt.Fprintf(os.Stderr, "scan negative balances failed: %v\n", err)
			os.Exit(2)
		}
		res.NegativeBalanceAccts = append(res.NegativeBalanceAccts, id)
	}
	if len(res.NegativeBalanceAccts) > 0 {
		res.Pass = false
	}

	// I3 — orphan txn_ids
	rowsI3, err := dbx.Query("SELECT txn_id::text FROM ledger_entries GROUP BY txn_id HAVING COUNT(*) != 2")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query I3 failed: %v\n", err)
		os.Exit(2)
	}
	defer rowsI3.Close()
	for rowsI3.Next() {
		var txnID string
		if err := rowsI3.Scan(&txnID); err != nil {
			fmt.Fprintf(os.Stderr, "scan I3 failed: %v\n", err)
			os.Exit(2)
		}
		res.OrphanTxnIDs = append(res.OrphanTxnIDs, txnID)
	}
	if len(res.OrphanTxnIDs) > 0 {
		res.Pass = false
	}

	fmt.Println("=== Verifier Report ===")
	printCheck("I1 ledger sum == 0          ", res.LedgerSum == 0, fmt.Sprintf("sum=%d", res.LedgerSum))
	printCheck("I2 no balance drift         ", len(res.DriftedAccounts) == 0, fmt.Sprintf("drifted=%v", res.DriftedAccounts))
	printCheck("    no negative balances    ", len(res.NegativeBalanceAccts) == 0, fmt.Sprintf("negative=%v", res.NegativeBalanceAccts))
	printCheck("I3 all txns have 2 entries  ", len(res.OrphanTxnIDs) == 0, fmt.Sprintf("orphans=%v", res.OrphanTxnIDs))

	fmt.Println("\n--- JSON ---")
	jsonBytes, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json marshal failed: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(string(jsonBytes))

	if !res.Pass {
		os.Exit(1)
	}
	os.Exit(0)
}

// printCheck — pretty status line.
// ✓ if ok, ✗ otherwise.
func printCheck(name string, ok bool, detail string) {
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Printf("  %s %s  %s\n", mark, name, detail)
}
