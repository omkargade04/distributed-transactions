# Experiment 05 — Postgres Restart Mid-Load

**Version:** v1
**Failure mode:** infra crash — DB process restart
**Date:** 2026-05-31
**Duration:** ~10 minutes

---

## Hypothesis

### Q1 — What happens to in-flight requests during the restart?

**Prediction: Requests during downtime return 500 (not 503).**

v1 has no load balancer — 503 requires a proxy. What actually happens:

1. `docker restart postgres` sends SIGTERM to Postgres → graceful shutdown → Postgres closes all connections, sends termination to clients
2. pgx pool detects connections closed (EOF / connection reset)
3. Requests during downtime: app tries to use dead pool connection OR open new one → both fail → handler catches DB error → returns **500**
4. While Postgres is restarting (~2-5s): every request returns 500
5. After Postgres back up: pool opens fresh connections → requests succeed again

Data persists: `pgdata` named volume in docker-compose.yml. Postgres restart does NOT wipe data.

### Q2 — Does the app auto-reconnect when Postgres comes back?

**Prediction: Yes — automatically, built into Go's `database/sql` pool.**

Go's `database/sql` has lazy reconnect built in. No app-level retry logic needed for reconnect:

1. Postgres restarts and comes back up
2. Next request tries to use a pool connection
3. Pool detects dead connection on use (liveness check / error)
4. Pool automatically opens a new connection to Postgres
5. If Postgres is up → new connection succeeds → request proceeds normally

v1 reconnects transparently. **What v1 is missing is retry at the REQUEST level** — requests that arrived during the downtime fail with 500 and are dropped. Clients get 500 and never retry. That's the v2 lesson (retry + backoff), not reconnect itself.

### Q3 — What does DB state look like after Postgres restarts?

**Prediction: Perfectly consistent. Verifier passes.**

- `pgdata` named volume → all committed data survives restart
- Postgres WAL: on shutdown, Postgres rolls back any uncommitted in-flight transactions (same guarantee as experiment 02)
- After restart: only fully committed transfers visible, no partial state possible
- Verifier: all invariants hold ✓

### Q4 — How is this different from experiment 02 (app kill)?

| | Exp 02 — app kill | Exp 05 — DB restart |
|--|--|--|
| What dies | App process | Postgres process |
| What stays running | Postgres | App process |
| Client sees | Connection refused to APP | 500 from running app (DB error inside handler) |
| DB consistency | ✓ WAL rollback | ✓ WAL rollback |
| Recovery | Manual: `docker compose up -d payment-api` | Automatic: pool reconnects on next request |
| App state after | Fresh process | Same process, stale pool connections cleared lazily |
| Error origin | Network layer (TCP refused) | Application layer (500 from handler) |

Key distinction: in exp 02, app is the **victim** — clients can't even reach it. In exp 05, app is the **middleman** — still running, still accepting HTTP connections, but its backend is down so it returns 500 to every caller.

---

## Reproduction

### Pre-conditions
- Fresh DB state (`make reset && sleep 10`)
- Stack healthy

### Steps

```bash
# Start load
RPS=30 DURATION=60s EXPERIMENT_ID=05 make sim &

# Mid-run: restart Postgres (~10s in)
sleep 10
docker restart postgres

# Wait for sim to finish
wait

# Verify DB state
make verify
curl -s http://localhost:8080/health

# Check logs for error pattern during downtime
docker logs payment-api 2>&1 | grep '"level":"ERROR"' | head -5 | jq .
```

---

## Observed

### Simulator summary
```json
{
  "sent": 1799,
  "completed_2xx": 1535,
  "rejected_4xx": 0,
  "failed_5xx": 264,
  "p50_ms": 6,
  "p95_ms": 15,
  "p99_ms": 45,
  "actual_rps": 29.98
}
```
264 failures (14.7%) during Postgres downtime. Pool auto-reconnected after restart — remaining 1535 requests all succeeded.

### Three error phases in app logs

**Phase 1 — Postgres graceful shutdown**
```json
{"msg":"transfer.failed","error":"begin: FATAL: terminating connection due to administrator command (SQLSTATE 57P01)"}
```
Postgres sent explicit termination to existing pool connections before stopping.

**Phase 2 — Postgres down**
```json
{"msg":"transfer.failed","error":"begin: failed to connect ... dial tcp 172.27.0.2:5432: connect: connection refused"}
```
Postgres fully stopped. New connection attempts refused immediately.

**Phase 3 — Docker DNS not ready (unexpected)**
```json
{"msg":"transfer.failed","error":"begin: failed to connect ... hostname resolving error: lookup postgres on 127.0.0.11:53: no such host"}
```
Postgres container came back up, but Docker's overlay network DNS (`127.0.0.11`) hadn't re-registered the container yet. App couldn't even resolve the hostname `postgres`. Docker-specific failure mode — doesn't happen in native deployments.

### Verifier output
```
  ✓ I1 ledger sum == 0
  ✓ I2 no balance drift
  ✓     no negative balances
  ✓ I3 all txns have 2 entries
  pass: true
```
All invariants hold. Data persisted on `pgdata` volume.

---

## Root cause

Three-phase failure, each with a different cause:

1. **SQLSTATE 57P01** — Postgres graceful shutdown sends `ErrorResponse` to all connected clients. pgx surfaces this as an error on the next call using that connection. In-flight `BeginTx()` calls fail immediately.

2. **connection refused** — Postgres process stopped, port 5432 not listening. New connection attempts fail at TCP level before any Postgres protocol is exchanged.

3. **DNS resolution failure** — Docker Compose uses an internal DNS server at `127.0.0.11`. When a container stops, its hostname is deregistered. When it restarts, there's a brief window before the hostname is re-registered. App gets `no such host` instead of `connection refused`. This is a container networking artifact, not a Postgres or app bug.

All three fail at `BeginTx()` — same error path in `transfer.go`, same 500 to caller.

After Docker DNS re-registers `postgres` hostname and Postgres accepts connections: `database/sql` pool automatically opens a fresh connection on the next request. No app code change needed for reconnect.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — `SUM(ledger) == 0` | ✓ holds | `pgdata` volume → all committed data survived restart. WAL rolled back in-flight txns on shutdown. |
| I2 — balance matches ledger | ✓ holds | Same as I1. |
| I3 — all txn_ids have 2 rows | ✓ holds | No partial transactions persisted. |
| app — no negative balance | ✓ holds | No race conditions triggered during this experiment. |
| **service availability** | ✗ broken | 264 requests (14.7%) dropped during ~5s downtime window. Clients got 500 with no retry signal. |

---

## Lesson preview

**Broke because:** no request-level retry. App pool reconnects automatically (built-in), but requests that arrived during Postgres downtime fail with 500 and are dropped. Clients have no way to know whether to retry.

**Fixed in: v2** — retry with exponential backoff + idempotency key. Client retries on 500. Server uses idempotency key to deduplicate if original request committed before the crash (same as exp 02 fix). Same mechanism, different trigger.

---

## Reflection

The three-phase error sequence was unexpected — predicted one error type, observed three distinct ones with different causes. The DNS resolution error (phase 3) was the most surprising: even after Postgres was back, the app couldn't reach it because Docker's internal DNS hadn't registered the container yet. This is a container-specific failure mode that wouldn't exist with a traditional VM or bare-metal deployment.

Prediction on auto-reconnect was correct: pool reconnected without any app-level retry code. But the REQUESTS during downtime were still dropped — the distinction between pool reconnect (infrastructure) and request retry (application) is now clear.

Comparing to experiment 02: in exp 02 the app is the victim (can't receive HTTP). In exp 05 the app receives every request but silently drops 14.7% of them by returning 500. From a client perspective, exp 05 is actually harder to handle — the service is still "up" (health endpoint returns 200 again after recovery), but you don't know if your payment landed or not. At least in exp 02, you know the server was unreachable.
