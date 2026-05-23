CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    balance_minor BIGINT NOT NULL,
    currency      CHAR(3) NOT NULL DEFAULT 'USD',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    txn_id       UUID NOT NULL,
    account_id   TEXT NOT NULL REFERENCES accounts(id),
    amount_minor BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- DELIBERATELY no indexes beyond PK in v1 (v3 lesson).
-- DELIBERATELY no CHECK (balance_minor >= 0) (must demonstrate race condition).
