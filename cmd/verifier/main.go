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

	// TODO: implement everything below this line
	_ = sql.Open
	_ = json.MarshalIndent
	_ = *initialBalance

	fmt.Println("verifier not implemented")
	os.Exit(2)
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
