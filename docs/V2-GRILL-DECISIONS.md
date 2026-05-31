# v2 Grill Decisions — Distributed Payment System Learning Project

**Captured:** 2026-05-31
**Source:** /grill-me interview session, 5 questions, all resolved
**Purpose:** Single source of truth for v2 implementation. `docs/v2-spec.md` derived from this document.

---

## Project Context (v2 delta from v1)

- **Goal:** Fix the client-uncertainty + duplicate-charge problems exposed by v1 experiments 01, 02, 05
- **NOT in v2:** race condition (03 → v3), pool exhaustion (04 → v3), outbox (v5), Prometheus (v4)
- **Tech stack delta:** add `github.com/golang-migrate/migrate/v4` for schema migrations
- **No new binaries.** No new services. Same monolith + Postgres + Docker Compose.

---

## Q1 — Idempotency key: location, lifecycle, replay semantics

### Location: HTTP header `Idempotency-Key`
- Stripe convention. Industry standard.
- Pure operation identity, decoupled from request body.
- Middleware can extract without parsing body twice.

### Format
- UUID v4, client-generated.
- 128-bit, collision-free, no coordination required.
- Max length 255 chars (Postgres TEXT comfortable).

### TTL: 24 hours
- Long enough to survive multi-hour outages + retry storms after.
- Short enough to bound table size.
- After 24h: same key treated as new request (fresh execution).

### Required vs optional: OPTIONAL in v2
- Backward-compat with v1 clients.
- If absent: server generates one (operation works, but not retry-safe).

### Replay response shape
- Same HTTP status as original
- Same body as original
- Extra response header: `Idempotency-Replay: true`
- Caller logic unchanged. Detection possible via header.

### In-flight duplicate (first call still running)
- Return **409 Conflict**
- Body: `{"error":"request_in_progress"}`
- Header: `Retry-After: 1`
- Do NOT block HTTP connection. Fail-fast → client retries shortly.

### Rejected
- ❌ Body field instead of header (mixes data with metadata)
- ❌ Server-only generation (defeats retry purpose)
- ❌ TTL=forever (unbounded table growth — v6 sharding lesson)
- ❌ Replay returns different body (breaks idempotency contract)

---

## Q2 — Conflict handling matrix

| Scenario | Server response | Reason |
|----------|----------------|--------|
| Same key + identical payload, original COMPLETED | 200 + cached response + `Idempotency-Replay: true` | Standard replay |
| Same key + identical payload, original IN-FLIGHT | 409 Conflict + `Retry-After: 1` | Don't block conn waiting |
| Same key + DIFFERENT payload | **422 Unprocessable Entity** + `{"error":"idempotency_key_conflict"}` | Caller bug. Loud failure. |
| Same key + original FAILED (5xx) | Re-execute (treat as new) | Don't cache transient failures |

### Payload comparison
- Hash incoming body with SHA-256 → compare to stored `request_hash` column
- Canonical JSON (sorted keys, no whitespace) before hashing
- Exclude `idempotency_key` itself from hash (key identifies intent, not part of it)

### Status state machine

```
                    ┌─────────┐
          insert ──→│ pending │
                    └────┬────┘
                         │ ledger commit
              ┌──────────┴──────────┐
              ↓                     ↓
        ┌───────────┐         ┌────────┐
        │ completed │         │ failed │
        └───────────┘         └────┬───┘
                                   │ on retry: DELETE + re-insert pending
                                   ↓
                              [back to pending]
```

- `failed` row gets DELETED on retry (not UPDATED). Simpler state.
- v2 doesn't track attempt count (defer to later versions if useful).

### Rejected
- ❌ Block on in-flight (long polling — HTTP timeouts cascade)
- ❌ Cache failed responses (permanent failure for one transient blip)
- ❌ Silently accept different payload (silently double-charges — worse than v1)
- ❌ JSONB equality compare (slow, whitespace/order sensitive)

---

## Q3 — Schema + migration strategy

### New `transfers` table

```sql
CREATE TABLE IF NOT EXISTS transfers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key   TEXT NOT NULL UNIQUE,
    request_hash      BYTEA NOT NULL,                    -- SHA-256 (32 bytes)
    request_payload   JSONB NOT NULL,
    response_status   INT,
    response_payload  JSONB,
    status            TEXT NOT NULL DEFAULT 'pending',   -- pending|completed|failed
    txn_id            UUID,                              -- soft ref to ledger_entries.txn_id
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error_message     TEXT
);

CREATE INDEX ON transfers (created_at) WHERE status != 'pending';
```

### Column rationale

| Column | Purpose |
|--------|---------|
| `idempotency_key` | UNIQUE = DB-level dedup. Insert collision = duplicate. |
| `request_hash` | Fast diff-payload check (Q2 scenario C). BYTEA = 32 bytes. |
| `request_payload` | Debug + audit trail. |
| `response_status` + `response_payload` | Cached replay (Q1). |
| `status` | State machine. |
| `txn_id` | Soft ref to ledger. NULL until ledger commit succeeds. No FK (txn_id has 2 ledger rows = no UNIQUE possible). |
| `created_at` | TTL purge basis. |

### Migration tool: `golang-migrate`

Layout:
```
migrations/
├── 0001_init.up.sql        # v1 schema
├── 0001_init.down.sql
├── 0002_seed.up.sql        # v1 seed (idempotent)
├── 0002_seed.down.sql
├── 0003_transfers.up.sql   # v2 new
└── 0003_transfers.down.sql
```

Migration runs from app startup (before HTTP listener):
```go
if err := db.Migrate(cfg.DBURL); err != nil {
    slog.Error("db.migrate_failed", "error", err.Error())
    os.Exit(1)
}
```

Tracked in `schema_migrations` table (managed by library).

### Cleanup strategy
- Background purge job for expired idempotency keys → **deferred to v4** (needs worker queue)
- v2 ignores TTL enforcement. Table grows. Not material at experiment scale.

### Rejected
- ❌ Store idempotency on `ledger_entries` (wrong layer — ledger is accounting, transfers is API operation log)
- ❌ Hex string for `request_hash` (2x size of BYTEA)
- ❌ Track `attempt_count` (correctness-focused v2, not analytics)
- ❌ FK `txn_id REFERENCES ledger_entries(txn_id)` (impossible — txn_id appears 2x in ledger_entries, no UNIQUE possible)

---

## Q4 — Client retry + graceful shutdown

### Simulator retry policy

| Aspect | Value |
|--------|-------|
| Idempotency key scope | One UUID per **intent**, reused across retries |
| Max attempts | 3 (1 initial + 2 retries) |
| Backoff curve | Exponential: 200ms, 400ms, 800ms |
| Jitter | Full jitter: `sleep ± rand[0, sleep/2]` |
| Retry triggers | HTTP 5xx, connection errors (status=0), 408, 429 (respect `Retry-After`) |
| Do NOT retry | 4xx (except 408, 429) — esp. 400/404/422 |
| Per-attempt timeout | 5s |

### Updated JSONL output (per attempt, not per intent)

```json
{"ts":"...","idempotency_key":"...","attempt":1,"status":500,"latency_ms":2401,"retried":true,"final":false}
{"ts":"...","idempotency_key":"...","attempt":2,"status":200,"latency_ms":12,"retried":false,"final":true}
```

`attempt` + `retried` + `final` lets HTML reports visualize retry storms.

### Updated simulator summary counters

- `intents_sent` — unique idempotency keys generated
- `intents_completed` — keys that eventually got 2xx
- `intents_failed` — exhausted retries
- `requests_total` — including retries (≥ intents_sent)
- `replays_served` — count of `Idempotency-Replay: true` responses observed

### Server graceful shutdown

```go
// in cmd/payment-api/main.go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

go func() {
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        slog.Error("server.listen_failed", "error", err.Error())
        os.Exit(1)
    }
}()

slog.Info("server.start", "port", cfg.Port)

<-sigCh
slog.Info("server.shutdown.starting")

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := srv.Shutdown(ctx); err != nil {
    slog.Error("server.shutdown.error", "error", err.Error())
}
slog.Info("server.shutdown.complete")
```

### `docker-compose.yml` change

```yaml
payment-api:
  stop_grace_period: 35s   # > srv.Shutdown(30s) timeout, prevents premature SIGKILL
```

### `restart: "no"` policy STAYS
- Kill experiments still need crashes to stay crashed
- Graceful shutdown only protects against SIGTERM (normal stop), NOT SIGKILL (process panic / OOM)
- Experiments 02 + 05 still demonstrate forced termination paths

### Rejected
- ❌ Single attempt timeout = total simulator timeout (interacts badly with retries)
- ❌ Infinite retries (masks broken systems)
- ❌ Retry on 4xx (wastes resources, hides client bugs)
- ❌ No jitter (synchronized thundering herd after recovery)
- ❌ Auto-restart payment-api (would mask kill experiments)

---

## Q5 — Experiments + outbox decision

### Outbox: **DEFERRED to v5**

Reasoning: outbox solves atomic (state change + event publish). v2 has zero downstream consumers. Building it now = plumbing with nowhere to drain. Earns its place at v5 (Kafka).

### v2 experiments (5 total)

| # | Title | Type | Pass criteria |
|---|-------|------|---------------|
| 01 | Duplicate request (rerun) | rerun | Second returns 200 + `Idempotency-Replay: true`. Same `txn_id`. ONE ledger entry pair. acc_001 charged once. |
| 02 | App kill mid-txn (rerun w/ retry) | rerun | Killed in-flight requests retry on new app. Second attempt either replays (if committed before kill) or executes fresh (if rolled back). **0 lost intents**. |
| 05 | Postgres restart (rerun w/ retry) | rerun | All intents complete eventually. Verifier passes. 0 lost intents. |
| 06 | Idempotency conflict (same key, diff payload) | **new** | Returns 422 `idempotency_key_conflict`. Only payload A executes. |
| 07 | Concurrent same-key requests | **new** | 5 simultaneous POSTs w/ identical key + body. Exactly 1 executes. Others get 409 → retry → cached replay. acc_001 charged ONCE. |

### NOT in v2 experiments
- 03 race condition (still broken, fix v3)
- 04 pool exhaustion (still broken, fix v3)
- Graceful shutdown standalone — covered by rerun of exp 02 (fewer drops than v1 baseline)

### v2 done definition (7 sections, mirrors v1)

1. **Code complete:** new `transfers` table + idempotency middleware + retry in simulator + graceful shutdown
2. **Happy path:** basic transfer still works; idempotency replay confirmed via curl
3. **5 experiments documented** (3 reruns + 2 new) in `experiments/v2/`
4. **Observability:** `idempotency_key` field added to relevant log events; new events `transfer.replayed`, `idempotency.conflict`
5. **Docs:** `docs/v2-spec.md` matches built code; `experiments/v2/README.md` indexes 5; `reflection.md` covers v2 lessons
6. **Migration reliability:** `make reset` (fresh DB) + `docker compose restart` (existing DB) both work cleanly
7. **Reflection captured** in `experiments/v2/reflection.md`

### Estimated time
~70% of v1 effort. Less new code (no new binaries), more careful semantics around idempotency edge cases + migration tooling.

### Rejected
- ❌ Add outbox preemptively (premature, no consumers)
- ❌ Skip experiments 06+07 (only these test v2-specific logic)
- ❌ Skip migration tool (schema drift accumulates fast)
- ❌ Retry on 4xx idempotency conflict (defeats purpose)
- ❌ Cache idempotency lookup in Go memory (process restart loses it; DB always source of truth)

---

## End-of-Grilling Status

- All 5 questions resolved
- 0 open questions
- Ready to write `docs/v2-spec.md`

**Next action:** spec written, scaffold stubs, user codes, claude reviews.
