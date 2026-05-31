CREATE TABLE IF NOT EXISTS transfers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key   TEXT NOT NULL UNIQUE,
    request_hash      BYTEA NOT NULL,
    request_payload   JSONB NOT NULL,
    response_status   INT,
    response_payload  JSONB,
    status            TEXT NOT NULL DEFAULT 'pending',
    txn_id            UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error_message     TEXT,
    CHECK (status IN ('pending', 'completed', 'failed'))
);

-- Partial index for the cleanup query (v4 will purge expired completed/failed rows)
CREATE INDEX idx_transfers_cleanup ON transfers (created_at)
    WHERE status != 'pending';
