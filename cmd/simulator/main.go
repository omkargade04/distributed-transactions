package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// transferReq mirrors the JSON shape payment-api accepts.
type transferReq struct {
	PayerID     string `json:"payer_id"`
	PayeeID     string `json:"payee_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// record is one row of the JSONL output file. Used by HTML reports for chart data.
// record is one row of the JSONL output — emitted PER ATTEMPT (not per intent).
//
// v2 additions: IdempotencyKey, Attempt, Retried, Final
type record struct {
	TS             string `json:"ts"`
	IdempotencyKey string `json:"idempotency_key,omitempty"` // v2: same across retries of one intent
	Attempt        int    `json:"attempt,omitempty"`         // v2: 1, 2, 3...
	Payer          string `json:"payer"`
	Payee          string `json:"payee"`
	Amount         int64  `json:"amount"`
	Status         int    `json:"status"`     // HTTP status, or 0 if request errored before reply
	LatencyMs      int64  `json:"latency_ms"`
	Retried        bool   `json:"retried,omitempty"` // v2: true if a retry was attempted after this record
	Final          bool   `json:"final,omitempty"`   // v2: true if this is the terminal attempt for this intent
	Replayed       bool   `json:"replayed,omitempty"` // v2: true if server returned Idempotency-Replay: true
	Error          string `json:"error,omitempty"`
}

// isRetryable returns true if this status/error should trigger another attempt.
//
// TODO (you): implement.
//
// Retry on:
//   - err != nil           (connection error, timeout)
//   - status == 0          (no response received)
//   - status >= 500        (server error — could be transient)
//   - status == 408        (request timeout)
//   - status == 429        (rate limited — should also honor Retry-After header)
//
// Do NOT retry on:
//   - any other 4xx — esp. 400, 404, 422 (idempotency conflict — caller bug)
//
// Returning false from this function = intent is terminal (success or hard failure).
func isRetryable(status int, err error) bool {
	// TODO: implement
	return false
}

// Simulator emits concurrent transfers at a target RPS.
//
// TODO (you): implement.
//
// Architecture overview:
//
//     [ticker tick] → [dispatcher goroutine] → [jobs channel] → [N workers]
//                                                                    ↓
//                                                              POST /v1/transfer
//                                                                    ↓
//                                                              [records channel] → [writer goroutine] → JSONL file
//
//   - Ticker fires every 1/RPS to control rate.
//   - Worker pool of --workers goroutines handles concurrency.
//   - Each worker picks random payer/payee from --accounts pool, random amount, POSTs.
//   - Each request → 1 record line in JSONL output.
//   - At end: print summary with sent / 2xx / 4xx / 5xx counts + p50/p95/p99 latency.
//
// Flags to support:
//   --target=<url>           default "http://localhost:8080"
//   --rps=<int>              default 100
//   --workers=<int>          default 10
//   --duration=<duration>    default 60s
//   --seed=<int64>           default time.Now().UnixNano() — deterministic w/ explicit value
//   --accounts=<int>         default 100
//   --output=<path>          optional JSONL path; if empty, no file written
//
// Steps to implement:
//
//   1. flag.Parse(), seed rng = rand.New(rand.NewSource(seed)).
//   2. Build []string of account IDs: "acc_001" .. "acc_<accounts>".
//      Hint: fmt.Sprintf("acc_%03d", i+1)
//   3. If --output set, os.Create the file. Defer Close.
//   4. ctx, cancel := context.WithTimeout(context.Background(), duration). Defer cancel.
//   5. httpClient := &http.Client{Timeout: 10 * time.Second}
//   6. Ticker for pacing: time.NewTicker(time.Second / time.Duration(rps))
//   7. Records channel: chan record, buffered 1024. Start writer goroutine reading from it.
//   8. Jobs channel: chan struct{}, buffered workers*2. Start N worker goroutines.
//   9. Dispatcher goroutine: for ticker.C, push struct{}{} to jobs (non-blocking via select default).
//      On ctx.Done(), close(jobs) and return.
//  10. Each worker:
//        a. Pick p1, p2 from accountIDs (ensure p1 != p2)
//        b. amount := int64(rng.Intn(5000) + 1)
//        c. body, _ := json.Marshal(transferReq{...})
//        d. start := time.Now()
//        e. resp, err := httpClient.Post(target+"/v1/transfer", "application/json", bytes.NewReader(body))
//        f. latency := time.Since(start).Milliseconds()
//        g. Build record, set status/error appropriately
//        h. Send record to records channel
//        i. atomic.AddInt64 counters: sent, completed2xx, rejected4xx, failed5xx
//        j. Append latency to a slice protected by a mutex (for percentile calc)
//
//  11. Wait for all workers (sync.WaitGroup). Close records channel after.
//  12. Wait for writer goroutine.
//  13. Sort latencies, compute p50/p95/p99 (use percentile() helper below).
//  14. Print summary JSON to stdout.
//
// COMMON PITFALLS:
//   - Forgetting to drain resp.Body → leaks connections. Always: io.Copy(io.Discard, resp.Body); resp.Body.Close().
//   - Using fmt.Fprintf to file w/o buffering → slow at high RPS. Direct Write is fine here.
//   - Sharing rand.Rand without lock → races. Either: one rng per worker, or wrap in mutex.
//   - Closing jobs channel from a worker → panic on send. Only dispatcher closes.
//   - Forgetting to handle ctx cancellation in workers → goroutines outlive program.
func main() {
	target := flag.String("target", "http://localhost:8080", "payment-api URL")
	rps := flag.Int("rps", 100, "target requests per second (intents/sec)")
	workers := flag.Int("workers", 10, "concurrent worker count")
	duration := flag.Duration("duration", 60*time.Second, "run duration")
	seed := flag.Int64("seed", time.Now().UnixNano(), "random seed (for reproducible runs)")
	accounts := flag.Int("accounts", 100, "size of pre-seeded account pool")
	output := flag.String("output", "", "JSONL output path (optional)")
	// v2 flags
	retries := flag.Int("retries", 3, "max attempts per intent (1 initial + N-1 retries)")
	backoffBaseMs := flag.Int("backoff-base-ms", 200, "base for exponential backoff in ms")
	perAttemptTimeout := flag.Duration("per-attempt-timeout", 5*time.Second, "per-HTTP-attempt timeout")
	_ = retries
	_ = backoffBaseMs
	_ = perAttemptTimeout
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	accountIDs := make([]string, *accounts)
	for i := range accountIDs {
		accountIDs[i] = fmt.Sprintf("acc_%03d", i+1)
	}

	var file *os.File
	if *output != "" {
		var err error
		file, err = os.Create(*output)
		if err != nil {
			slog.Error("failed to create output file", "error", err)
			os.Exit(2)
		}
		defer file.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Ticker for pacing: time.NewTicker(time.Second / time.Duration(rps))
	ticker := time.NewTicker(time.Second / time.Duration(*rps))
	defer ticker.Stop()

	// Records channel: chan record, buffered 1024. Start writer goroutine reading from it.
	records := make(chan record, 1024)
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for r := range records {
			if file != nil {
				b, _ := json.Marshal(r)
				_, _ = file.Write(b)
				_, _ = file.Write([]byte("\n"))
			}
		}
	}()

	jobs := make(chan struct{}, *workers*2)

	var (
		// v1 counters — still emitted for backward compat with v1 reporting
		sent         int64
		completed2xx int64
		rejected4xx  int64
		failed5xx    int64
		latenciesMu  sync.Mutex
		latencies    []int64

		// v2 counters — track intents (unique idempotency keys) vs attempts (HTTP requests)
		// TODO (you): wire these in the worker loop.
		// intentsSent      int64  // one per outermost loop iteration
		// intentsCompleted int64  // intent eventually got 2xx
		// intentsFailed    int64  // intent exhausted retries OR got non-retryable 4xx
		// requestsTotal    int64  // HTTP attempts including retries (≥ intentsSent)
		// replaysServed    int64  // count of Idempotency-Replay: true responses
	)

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerIndex int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(workerIndex)))
			// v2: per worker = one intent per loop iteration. Each intent makes 1..N HTTP attempts.
			//
			// TODO (you): wrap the existing single-attempt HTTP code in a retry loop.
			//
			// for range jobs {
			//     // (existing pick-payer/payee/amount + build body)
			//     intentKey := uuid.NewString()                   // one key per intent (per Q4)
			//
			//     for attempt := 1; attempt <= *retries; attempt++ {
			//         attemptCtx, cancel := context.WithTimeout(ctx, *perAttemptTimeout)
			//         req, _ := http.NewRequestWithContext(attemptCtx, "POST", *target+"/v1/transfer", bytes.NewReader(body))
			//         req.Header.Set("Content-Type", "application/json")
			//         req.Header.Set("Idempotency-Key", intentKey)
			//
			//         resp, err := httpClient.Do(req)
			//         latency := time.Since(reqStart).Milliseconds()
			//         cancel()
			//
			//         // build record (set IdempotencyKey, Attempt, Status, LatencyMs, Replayed)
			//         // emit record via records channel
			//
			//         // check if retry needed
			//         needRetry := isRetryable(rec.Status, err) && attempt < *retries
			//         rec.Retried = needRetry
			//         rec.Final = !needRetry
			//         records <- rec
			//
			//         if !needRetry {
			//             // intent terminal
			//             if rec.Status >= 200 && rec.Status < 300 { atomic.AddInt64(&intentsCompleted, 1) }
			//             else                                       { atomic.AddInt64(&intentsFailed, 1)    }
			//             break
			//         }
			//
			//         // exponential backoff with full jitter
			//         sleep := time.Duration(*backoffBaseMs) * time.Millisecond * (1 << (attempt - 1))
			//         jitter := time.Duration(rng.Int63n(int64(sleep / 2)))
			//         time.Sleep(sleep + jitter)
			//     }
			//
			//     atomic.AddInt64(&intentsSent, 1)
			// }
			for range jobs {
				p1 := accountIDs[rng.Intn(len(accountIDs))]
				p2 := accountIDs[rng.Intn(len(accountIDs))]
				for p1 == p2 {
					p2 = accountIDs[rng.Intn(len(accountIDs))]
				}
				amount := int64(rng.Intn(5000) + 1)

				body, _ := json.Marshal(transferReq{
					PayerID:     p1,
					PayeeID:     p2,
					AmountMinor: amount,
					Currency:    "USD",
				})

				reqStart := time.Now()
				resp, err := httpClient.Post(*target+"/v1/transfer", "application/json", bytes.NewReader(body))
				latency := time.Since(reqStart).Milliseconds()

				rec := record{
					TS:        time.Now().UTC().Format(time.RFC3339Nano),
					Payer:     p1,
					Payee:     p2,
					Amount:    amount,
					LatencyMs: latency,
				}

				if err != nil {
					rec.Status = 0
					rec.Error = err.Error()
					atomic.AddInt64(&failed5xx, 1)
				} else {
					rec.Status = resp.StatusCode
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()

					switch {
					case resp.StatusCode >= 200 && resp.StatusCode < 300:
						atomic.AddInt64(&completed2xx, 1)
					case resp.StatusCode >= 400 && resp.StatusCode < 500:
						atomic.AddInt64(&rejected4xx, 1)
					default:
						atomic.AddInt64(&failed5xx, 1)
					}
				}

				select {
				case records <- rec:
				case <-ctx.Done():
				}

				atomic.AddInt64(&sent, 1)

				latenciesMu.Lock()
				latencies = append(latencies, latency)
				latenciesMu.Unlock()
			}
		}(w)
	}

	runStart := time.Now()
	go func() {
		for {
			select {
			case <-ctx.Done():
				close(jobs)
				return
			case <-ticker.C:
				select {
				case jobs <- struct{}{}:
				default:
				}
			}
		}
	}()

	wg.Wait()
	close(records)
	writerWg.Wait()

	totalDuration := time.Since(runStart)

	latenciesMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := percentile(latencies, 50)
	p95 := percentile(latencies, 95)
	p99 := percentile(latencies, 99)
	latenciesMu.Unlock()

	summary := map[string]any{
		"event":         "simulator.summary",
		"sent":          atomic.LoadInt64(&sent),
		"completed_2xx": atomic.LoadInt64(&completed2xx),
		"rejected_4xx":  atomic.LoadInt64(&rejected4xx),
		"failed_5xx":    atomic.LoadInt64(&failed5xx),
		"p50_ms":        p50,
		"p95_ms":        p95,
		"p99_ms":        p99,
		"duration_s":    totalDuration.Seconds(),
		"actual_rps":    float64(sent) / totalDuration.Seconds(),
		"seed":          *seed,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
}

// percentile returns the value at the given percentile in a sorted slice.
// Returns 0 if slice empty.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
