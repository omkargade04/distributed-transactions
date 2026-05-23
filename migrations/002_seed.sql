-- Pre-seed 100 accounts: acc_001..acc_100, each at balance_minor=100000 ($1000).
-- Idempotent — re-runnable without error.
INSERT INTO accounts (id, balance_minor)
SELECT 'acc_' || lpad(g::text, 3, '0'), 100000
FROM generate_series(1, 100) AS g
ON CONFLICT (id) DO NOTHING;
