package db

// SQL query constants. All parameterized — never string-concat user input.
//
// Convention: Q prefix + verb + noun.

const (
	// QGetAccount returns id, balance_minor, currency for a single account.
	// $1 = account id
	QGetAccount = `
		SELECT id, balance_minor, currency
		FROM accounts
		WHERE id = $1
	`

	// QGetAccountForUpdate is identical to QGetAccount but acquires a row-level
	// lock (FOR UPDATE) held until the surrounding transaction commits or rolls back.
	// Use for the payer balance read inside transfer transactions to prevent the
	// concurrent same-payer race condition (v1 experiment 03).
	//
	// The lock causes a second concurrent transfer from the same payer to BLOCK
	// at this SELECT until the first transaction commits. The second then re-reads
	// the committed (lower) balance and correctly fails the balance check.
	QGetAccountForUpdate = `
		SELECT id, balance_minor, currency
		FROM accounts
		WHERE id = $1
		FOR UPDATE
	`

	// QUpdateBalance applies a signed delta to balance_minor.
	// $1 = delta (signed: negative for debit, positive for credit)
	// $2 = account id
	QUpdateBalance = `
		UPDATE accounts
		SET balance_minor = balance_minor + $1,
		    updated_at    = now()
		WHERE id = $2
	`

	// QInsertLedgerEntry inserts one row of a debit/credit pair.
	// $1 = txn_id (UUID grouping debit+credit)
	// $2 = account_id
	// $3 = amount_minor (signed)
	QInsertLedgerEntry = `
		INSERT INTO ledger_entries (txn_id, account_id, amount_minor)
		VALUES ($1, $2, $3)
	`
)
