# v1 Grill Decisions — Distributed Payment System Learning Project

**Captured:** 2026-05-23
**Source:** /grill-me interview session, 10 questions, all resolved
**Purpose:** Single source of truth for v1 implementation. The `docs/v1-spec.md` will be derived from this document.

---

## Project Context

- **Goal:** Learn backend distributed systems by building a payment processing system layer by layer (v1 → v8). Each version introduces ONE new failure mode + corresponding fix.
- **Domain:** P2P wallet transfer with double-entry ledger (Venmo/PayPal model)
- **Tech stack:** Go + Postgres + Docker Compose (v1). Add Redis + Kafka + Prometheus + Grafana + Jaeger in later versions.
- **Money representation:** Integer minor units (cents). Never float.
- **Companion reading:** "Designing Data-Intensive Applications"

---

## Q1 — Scope Boundary (v1 = primitive monolith)

### IN
- 1 HTTP endpoint: `POST /transfer` (payer_id, payee_id, amount_minor)
- 1 endpoint: `GET /accounts/{id}` (balance lookup)
- 1 endpoint: `GET /health`
- Postgres with 2 tables (`accounts`, `ledger_entries`)
- Single Go binary, no internal modules
- Simulator binary: emits N transfers at fixed RPS
- Verifier binary: checks invariants
- Docker Compose: app + Postgres only
- Structured JSON logs to stdout

### OUT (deferred)
- ❌ Idempotency keys (v2)
- ❌ Retry / outbox (v2)
- ❌ Auth / users / signup (never — pre-seed accounts)
- ❌ Multi-currency (never — USD only)
- ❌ Refund / reversal (v3+)
- ❌ Connection pooling tuning (v3)
- ❌ Indexes beyond PK (v3)
- ❌ Metrics / traces (v4)
- ❌ Automated tests (v2)
- ❌ Migrations tool (v1 = raw SQL file at startup)
- ❌ Graceful shutdown (v1 = crash teaches lesson)

### Rationale
v1 should FEEL primitive. When you crash it, the gap must be obvious. If v1 already has idempotency, you can't demonstrate why idempotency exists.

---

## Q2 — API Protocol: REST + JSON

### Endpoint shapes

```
POST /v1/transfer
  body: {"payer_id": "acc_001", "payee_id": "acc_002", "amount_minor": 1500, "currency": "USD"}
  resp 200: {"txn_id": "uuid", "status": "completed"}
  resp 400: {"error": "insufficient_funds"}
  resp 404: {"error": "account_not_found"}
  resp 500: {"error": "internal"}

GET /v1/accounts/{id}
  resp 200: {"id": "acc_001", "balance_minor": 50000, "currency": "USD"}

GET /health
  resp 200: {"status": "ok"}
```

### Status codes used in v1
- 200 — success
- 400 — validation error / insufficient_funds
- 404 — account missing
- 500 — server error

NO 201/202/422 distinctions in v1.

### Why REST not gRPC
- Curl-debuggable, no protoc step, browser/Postman testable
- Protocol ≠ distributed systems lesson
- gRPC legitimate at v5/v6 when internal service-to-service calls appear

---

## Q3 — Schema

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE accounts (
    id            TEXT PRIMARY KEY,
    balance_minor BIGINT NOT NULL,
    currency      CHAR(3) NOT NULL DEFAULT 'USD',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    txn_id       UUID NOT NULL,
    account_id   TEXT NOT NULL REFERENCES accounts(id),
    amount_minor BIGINT NOT NULL,           -- signed: -1500 debit, +1500 credit
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- NO indexes beyond PK in v1. Missing indexes = v3 lesson.
```

### Three key design choices
1. **Denormalized `balance_minor` on accounts** (vs computed from SUM ledger).
   - Real systems do this. Compute-from-ledger is O(N) per read.
   - Forces multi-row write: UPDATE accounts ×2 + INSERT ledger_entries ×2 in one DB txn.
   - Crash between = balance drifts from ledger. This is the v1 ACID lesson.
2. **No separate `transfers` table** in v1.
   - `txn_id` groups debit+credit pair in ledger_entries.
   - Invariant: SUM(amount_minor WHERE txn_id=X) = 0
   - v2 adds `transfers` table when idempotency_key needs a home.
3. **Signed `amount_minor`** (not separate debit/credit columns).
   - One column, sign indicates direction. SUM = 0 invariant trivial.

### Pre-seed strategy

```sql
INSERT INTO accounts (id, balance_minor)
SELECT 'acc_' || lpad(g::text, 3, '0'), 100000
FROM generate_series(1, 100) AS g
ON CONFLICT (id) DO NOTHING;
```

100 accounts (`acc_001` → `acc_100`), each $1000 starting balance. Idempotent.

### Invariants the verifier checks
```sql
-- I1: ledger sums to zero
SELECT SUM(amount_minor) FROM ledger_entries;  -- must = 0

-- I2: account balance matches ledger
SELECT a.id, a.balance_minor, COALESCE(SUM(l.amount_minor), 0) + 100000 AS expected
FROM accounts a LEFT JOIN ledger_entries l ON l.account_id = a.id
GROUP BY a.id
HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + 100000;
-- any row returned = drift bug

-- I3: no orphan ledger entries (every txn_id has exactly 2 rows in v1)
SELECT txn_id, COUNT(*) FROM ledger_entries GROUP BY txn_id HAVING COUNT(*) != 2;
```

### Explicitly NOT in v1 schema
- No `status` column on ledger_entries (v2)
- No `txn_type` (v1 only transfers)
- No CHECK constraint `balance_minor >= 0` (masks race condition we want to demonstrate)
- No `version` column for optimistic locking (v3)

---

## Q4 — Simulator v1 Scope

### IN
- Reads from pool of 100 pre-seeded accounts
- Picks random `payer_id` and `payee_id` (uniform, distinct)
- Random `amount_minor` in `[1, 5000]` (1¢ to $50)
- Concurrent worker pool: `--workers=N`
- Target RPS: `--rps=N` (token bucket pacing)
- Duration: `--duration=60s`
- Logs each request: payer, payee, amount, http_status, latency_ms
- Final summary: sent, 2xx, 4xx, 5xx, p50/p95/p99 latency, total duration

### CLI
```
simulator --target=http://localhost:8080 \
          --rps=100 --workers=10 \
          --duration=60s --seed=42 \
          --output=experiments/v1/data/0N-sim-requests.jsonl
```

### Output format (JSONL — one line per request)
```json
{"ts":"2026-05-23T15:42:01.234Z","payer":"acc_001","payee":"acc_002","amount":1500,"status":200,"latency_ms":12,"error":null}
```

### OUT (deferred)
- Poisson/burst distribution (v3+)
- Duplicate idempotency_key injection (v2)
- Malformed payload injection (v2)
- Same-payer hot-key targeting (v3)
- Chaos hooks / process kill (v5/v8)
- Replay mode (v8)
- Prometheus metrics (v7)
- Config file (CLI flags only)

### Key behaviors
- Concurrent workers (NOT sequential) — needed to surface races naturally
- NO retry on failure (v2 lesson)
- Same `--seed` = deterministic sequence (reproduces bugs)
- Does NOT track expected balance (verifier does that)

---

## Q5 — v1 Failure Modes to Demonstrate

Five experiments must run successfully (i.e., reproduce the failure consistently) for v1 to be done.

| # | Failure | How to trigger | Expected bad outcome | v2-v8 fix |
|---|---------|----------------|----------------------|-----------|
| 01 | Duplicate request | Curl same body twice | Money debited twice | v2 — idempotency keys |
| 02 | Process kill mid-txn | `docker kill payment-svc` during sim | In-flight requests 5xx | v2 — outbox / retry |
| 03 | Concurrent same-payer race | 2 simultaneous transfers from acc_001 | Negative balance OR overcommit | v3 — SELECT FOR UPDATE / serializable |
| 04 | DB connection pool exhaustion | Crank simulator to high RPS (500+) | All requests hang or 500 | v3 — pool tuning |
| 05 | Postgres restart mid-load | `docker restart postgres` during sim | App crashes, no auto-reconnect | v2 — retry + reconnect |

### Required artifact: verifier tool

Separate Go binary:
```
verifier --db=postgres://...
```
Output:
```
✓ ledger sum: 0
✓ all account balances match ledger sums
✗ accounts with negative balance: acc_007 (-340)
✗ orphan ledger entries (txn_id with != 2 rows): 3
```

### OUT (deferred)
- Network partition (v5)
- Slow consumer / backpressure (v4)
- Disk full (v8)
- Clock skew (v8)
- Replica lag (v6)
- Memory leak (v7)

### Failure modes proposed but deferred from v1
- OOM → v7/v8
- Disk full → v8
- Slow query (unindexed scan) → v3
- Huge SQL payload → v3/v7
- PII redaction in logs → v7 (security, not crash)

NEVER truncate SQL queries to fix slowness (corrupts data). Only truncate log lines.

---

## Q6 — Observability Baseline (v1 = logs only)

### Tech
- Go stdlib `log/slog` with JSON handler. Zero deps.

### Log line shape
```json
{
  "ts": "2026-05-23T15:42:01.234Z",
  "level": "info",
  "svc": "payment-api",
  "event": "transfer.completed",
  "txn_id": "550e8400-...",
  "payer_id": "acc_001",
  "payee_id": "acc_002",
  "amount_minor": 1500,
  "duration_ms": 12,
  "request_id": "req_abc123"
}
```

### Required fields every log line
- `ts` — RFC3339 with millis
- `level` — debug / info / warn / error
- `svc` — `payment-api` | `simulator` | `verifier`
- `event` — namespaced (`transfer.completed`, etc.)
- `request_id` — UUID per HTTP request, propagated via `X-Request-ID` header

### Conditional fields
- `txn_id` when known
- `error` when level=error (string only, no stack trace in v1)
- `duration_ms` for completed ops

### Required events
| Event | Where | Level |
|-------|-------|-------|
| `server.start` | main() | info |
| `server.shutdown` | signal handler | info |
| `request.received` | middleware | info |
| `request.completed` | middleware | info |
| `transfer.received` | handler entry | info |
| `transfer.validated` | after balance check | debug |
| `transfer.completed` | after commit | info |
| `transfer.rejected` | 4xx response | info |
| `transfer.failed` | 5xx response | error |
| `db.query.slow` | query > 100ms | warn |
| `db.error` | query failure | error |

### Forbidden
- ❌ Full request/response bodies (PII + bloat)
- ❌ Stack traces (v7)
- ❌ Pretty-printed multiline JSON

### Query path
```
docker logs payment-api | jq 'select(.event=="transfer.failed")'
```

### Configuration
- `--log-level=info|debug` flag

---

## Q7 — Repo Layout

```
distributed-payment-system/
├── README.md
├── docker-compose.yml
├── Makefile
├── .gitignore
├── .env.example
├── go.mod
├── go.sum
├── Dockerfile
│
├── cmd/                            # binary entry points
│   ├── payment-api/main.go
│   ├── simulator/main.go
│   └── verifier/main.go
│
├── internal/                       # private packages
│   ├── api/                        # HTTP handlers, routing, middleware
│   │   ├── handler.go
│   │   ├── middleware.go
│   │   └── server.go
│   ├── ledger/                     # domain logic
│   │   ├── transfer.go             # debit + credit in one DB txn
│   │   ├── account.go              # balance lookup
│   │   └── errors.go
│   ├── db/                         # DB connection + queries
│   │   ├── db.go
│   │   └── queries.go
│   └── config/
│       └── config.go
│
├── migrations/
│   ├── 001_init.sql
│   └── 002_seed.sql
│
├── experiments/v1/
│   ├── README.md
│   ├── 01-duplicate-request.md
│   ├── 02-process-kill.md
│   ├── 03-concurrent-race.md
│   ├── 04-pool-exhaustion.md
│   ├── 05-postgres-restart.md
│   ├── reflection.md
│   ├── data/
│   ├── _assets/
│   └── index.html
│
├── docs/
│   └── v1-spec.md
│
├── templates/
│   ├── experiment.html
│   └── experiment-template.md
│
└── scripts/
    ├── seed.sh
    ├── reset-db.sh
    ├── psql.sh
    ├── new-experiment.sh
    ├── render-experiment.sh
    └── render-all.sh
```

### Makefile essentials
```makefile
.PHONY: up down sim verify reset psql logs render-v1
up:        ; docker compose up -d
down:      ; docker compose down
sim:       ; go run ./cmd/simulator --target=http://localhost:8080 --rps=$(RPS) --duration=$(DURATION)
verify:    ; go run ./cmd/verifier --db=$(DB_URL)
reset:     ; ./scripts/reset-db.sh
psql:      ; ./scripts/psql.sh
logs:      ; docker logs -f payment-api | jq
render-v1: ; bash scripts/render-all.sh experiments/v1
```

### Key conventions
- `cmd/` for binaries, `internal/` for packages (standard Go layout)
- `internal/ledger/` (NOT `internal/payments/`) — domain language
- Migrations = raw SQL applied by Postgres on first start
- Per-experiment markdown isolation (one file each, linkable)
- Git tags `v1`, `v2`, ... for version snapshots
- Code evolves in place across versions, NOT forked

### Rejected
- Separate repos for app vs simulator
- Go workspaces (`go.work`) — single module
- `pkg/` (no public packages)
- `tests/` separate dir (Go convention is `_test.go` adjacent)
- `proto/`, `helm/`, `k8s/`, `vendor/`, `tools/`
- ORM (GORM/ent/sqlboiler) — raw SQL teaches what's happening
- `.github/workflows/` — no CI in v1

---

## Q8 — Docker Compose & Dockerfile

### `docker-compose.yml`
```yaml
services:
  postgres:
    image: postgres:16-alpine
    container_name: payment-postgres
    environment:
      POSTGRES_USER: payment
      POSTGRES_PASSWORD: payment_dev
      POSTGRES_DB: payment
      PGDATA: /var/lib/postgresql/data/pgdata
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./migrations:/docker-entrypoint-initdb.d:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U payment -d payment"]
      interval: 2s
      timeout: 2s
      retries: 10
    restart: unless-stopped

  payment-api:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: payment-api
    environment:
      DB_URL: "postgres://payment:payment_dev@postgres:5432/payment?sslmode=disable"
      PORT: "8080"
      LOG_LEVEL: "info"
    ports:
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy
    restart: "no"

volumes:
  pgdata:
```

### Critical choices
- `postgres:16-alpine` (NOT `latest`, NOT 17)
- App `restart: "no"` — DELIBERATE. Crashes must stay crashed for v1 lessons.
- Postgres `restart: unless-stopped` — DB recovers from accidental stop
- `depends_on.condition: service_healthy` — no "connection refused" noise on start
- Migrations mounted `:ro` at `/docker-entrypoint-initdb.d` — auto-runs on first start ONLY
- `sslmode=disable` — local Docker only (v5+ fixes)
- Password hardcoded — v1 has no secrets-mgmt lesson
- Named volume `pgdata` survives `down`; `down -v` nukes

### `Dockerfile` (multi-stage)
```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/payment-api ./cmd/payment-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/payment-api /usr/local/bin/payment-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/payment-api"]
```

### Reset workflow
```bash
# Nuke everything (including data)
docker compose down -v && docker compose up -d

# Restart app only, keep data
docker compose restart payment-api
```

### Host connection string
```
postgres://payment:payment_dev@localhost:5432/payment
```

### Resource limits — NOT in base v1
Added per failure-mode test only. Default v1 = unlimited (failure #4 needs no caps).

---

## Q9 — v1 "Done" Definition

v1 is DONE when all 7 sections check.

### 1. Code complete
- [ ] `cmd/payment-api`, `cmd/simulator`, `cmd/verifier` build clean (`go build ./...`)
- [ ] `docker compose up -d` brings up healthy stack in <30s on cold start
- [ ] `make sim RPS=50 DURATION=30s` runs without setup beyond `make up`
- [ ] `make verify` runs and prints invariant report

### 2. Happy path proven
- [ ] `curl POST /transfer` with valid payload → 200 + txn_id
- [ ] `curl GET /accounts/acc_001` → balance reflecting completed transfers
- [ ] After 30s sim @ 50 RPS: ledger SUM = 0, balances match ledger sums (verifier passes)
- [ ] p99 latency < 100ms under normal load (sanity baseline)

### 3. All 5 failure modes reproduced + documented
Each failure has `experiments/v1/0N-name.md` with:
- [ ] Exact reproduction command sequence
- [ ] Observable evidence (logs, HTTP responses, DB state)
- [ ] Verifier output showing broken invariant
- [ ] Hypothesis (predicted before run)
- [ ] Root cause analysis
- [ ] v2-v8 lesson preview

### 4. Observability baseline working
- [ ] Every request emits structured JSON log with `request_id`
- [ ] `docker logs payment-api | jq 'select(.event=="transfer.failed")'` filters cleanly
- [ ] No PII in logs

### 5. Documentation complete
- [ ] `docs/v1-spec.md` matches what was built (drift = bug)
- [ ] `README.md` Getting Started: `git clone` → `make up` → `make sim` → success in <3 min for fresh reader
- [ ] `experiments/v1/README.md` indexes all 5 failures

### 6. Reset reliability
- [ ] `docker compose down -v && docker compose up -d` returns to clean state every time
- [ ] No manual cleanup needed between experiment runs
- [ ] Seed always restores 100 accounts at $1000

### 7. Reflection captured
- [ ] `experiments/v1/reflection.md` written: surprises, what was harder than expected, what to do differently

### Non-criteria (deliberately NOT required)
- ❌ Automated test coverage (v2)
- ❌ Perf benchmarks beyond p99 < 100ms sanity (v7)
- ❌ Doc polish — clear + correct enough, not publication-ready
- ❌ Peer code review — solo project
- ❌ Security audit — universal hygiene yes, deep audit no

### Estimated time
- Build: 4-8 hrs (Go familiar) or 12-20 hrs (learning Go simultaneously)
- Crash experiments + docs: 4-6 hrs (each ~45-60 min)
- Total: 1-2 focused weekends OR 5-10 evenings

### Trigger to start v2
All 7 sections check → `git tag v1` → write `docs/v2-spec.md`.

DO NOT retroactively refactor v1 when v2 lessons emerge. v1 stands as historical "broken on purpose" record.

---

## Q10 — Experiment Journaling

### Per-experiment markdown template (7 sections)

Each `experiments/v1/0N-*.md` file follows:

```markdown
# Experiment 0N — <Short Title>

**Version:** v1
**Failure mode:** <category>
**Date:** YYYY-MM-DD
**Duration:** ~X minutes

---

## Hypothesis
One paragraph. What you predict will happen and why.

## Reproduction
### Pre-conditions
### Steps (copy-pasteable commands)

## Observed
### HTTP responses
### Log excerpt (5-10 representative lines)
### DB state (after)
### Verifier output

## Root cause
What actually happened at DB/code level.

## Invariant impact
Which invariants broke (I1 ledger sum, I2 balance vs ledger, I3 paired entries).

## Lesson preview
Maps to which v2-v8 version + the fix mechanism.

## Reflection
2-5 sentences. Surprises. What you'd check next time.
```

### Required artifacts per experiment
- `data/0N-sim-requests.jsonl` — raw simulator output
- `data/0N-verifier.json` — verifier output
- `data/0N-db-snapshot.sql` — `pg_dump` after the failure

### Why this template
- **Hypothesis** forces prediction BEFORE running — catches hindsight bias
- **Observed ≠ Root cause** separation — data vs interpretation, no conflation
- **Invariant impact** = precision over "system broke"
- **Lesson preview** builds narrative arc across versions
- **Reflection** = surprise tracking = highest-value learning

### Anti-patterns
- ❌ Writing docs at the end (memory fades — within 24h)
- ❌ Screenshots of terminals (use copy-pasted text)
- ❌ Skipping Hypothesis "because I know what'll happen"
- ❌ Massive log dumps inline (reference file, paste 5-10 lines max)

---

## Q10 Addendum — HTML Companion Reports (Auto-generated)

### Principle
Markdown is source of truth. HTML auto-generated, never hand-edited.

### Layout
```
experiments/v1/
├── 03-concurrent-race.md           ← source of truth
├── 03-concurrent-race.html         ← generated (don't edit)
├── data/
│   ├── 03-sim-requests.jsonl
│   ├── 03-verifier.json
│   └── 03-db-snapshot.sql
└── _assets/
    ├── report.css
    ├── chart-helpers.js
    └── mermaid.min.js
```

### Stack
| Layer | Tool | Size | Why |
|-------|------|------|-----|
| MD → HTML shell | Pandoc | CLI | Industry standard |
| Diagrams (flow/sequence/architecture) | Mermaid.js | ~500KB CDN | Text source, GitHub renders in md too |
| Charts (histogram, time-series, pie) | Chart.js | ~75KB CDN | Lean, no framework baggage |
| Styling | Hand-written CSS | <5KB | One file, all reports |
| Data | JSONL from simulator | varies | Append-friendly |

### Generator script
`scripts/render-experiment.sh <md-file>` — runs pandoc with template injecting Chart.js + Mermaid CDN; charts auto-render at page load by fetching JSONL data.

### Visual elements HTML adds
- Architecture diagram (Mermaid graph TD) at top
- Sequence diagram of failure flow (Mermaid sequenceDiagram) in Reproduction
- Latency histogram + status codes pie + RPS/error time-series (Chart.js) in Observed
- Color-coded log table (filterable)
- Side-by-side before/after DB state table with diff highlighting

### Index page
`experiments/v1/index.html` — auto-generated from `README.md`, card layout, click → experiment HTML.

### Cost
- One-time template setup: 2-3 hours
- Per experiment: zero extra work if md + jsonl exist
- `make render-v1` regenerates everything

### Rejected
- Hand-writing HTML (drift guaranteed by experiment 2)
- Full SSG (Hugo/Astro/MkDocs) — overkill for v1
- PDF generation (use browser print-to-pdf on demand)
- Plotly (Chart.js sufficient)
- Grafana embeds (no Grafana till v4)

---

## Cross-cutting Decisions (Referenced by Multiple Questions)

### Observability rollout (revised — brought forward to v4 from v7)
| Version | Observability added |
|---------|---------------------|
| v1 | Structured JSON logs (slog) |
| v2 | `request_id` propagation across calls |
| v3 | `pg_stat_statements`, slow query log, `EXPLAIN ANALYZE` workflow |
| **v4** | **Prometheus + Grafana + node_exporter + postgres_exporter + cAdvisor + Go pprof endpoint** |
| v5 | + Kafka exporter, Redis exporter, business metrics |
| v6 | + replication lag, shard distribution dashboard |
| v7 | + Jaeger/Tempo tracing, Loki log aggregation |
| v8 | + chaos run annotations on Grafana |

### Full version roadmap context
- v1: monolith + Postgres, single txn, no idempotency → break with duplicate
- v2: + idempotency + outbox + retry → break with DB crash mid-txn
- v3: DB tuning (index, pool, isolation, locks) → break with concurrent updates
- v4: + worker queue (DB-backed) + Prometheus + Grafana + pprof → break with slow worker
- v5: + Kafka + cache + circuit breaker → break with network partition
- v6: + sharding/replication → break with replica lag
- v7: load test + memory profile + distributed tracing (Jaeger) + log aggregation (Loki) → find real bottleneck
- v8: chaos engineering (kill -9, tc netem) → harden

---

## End-of-Grilling Status

- All 10 questions resolved
- 0 open questions
- Ready to write `docs/v1-spec.md` (which translates this document into implementation-ready form with file-by-file code skeletons)

**Next action:** await user kickoff to write v1 spec.
