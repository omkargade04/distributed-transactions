-- Indexes for ledger_entries query patterns.
-- account_id: verifier I2 query (balance drift check), per-account history lookups.
-- txn_id: verifier I3 query (orphan check), per-transaction lookups.
-- Without these, both verifier queries do full table scans (Seq Scan) on ledger_entries.
CREATE INDEX idx_ledger_account_id ON ledger_entries (account_id);
CREATE INDEX idx_ledger_txn_id     ON ledger_entries (txn_id);
