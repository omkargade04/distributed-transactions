-- pg_stat_statements tracks execution stats for all SQL statements.
-- Requires shared_preload_libraries=pg_stat_statements in postgres config (set in docker-compose).
-- Use to identify slow queries: SELECT * FROM pg_stat_statements ORDER BY mean_exec_time DESC;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
