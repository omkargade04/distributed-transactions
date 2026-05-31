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

	"github.com/google/uuid"
)

type transferReq struct {
	PayerID     string `json:"payer_id"`
	PayeeID     string `json:"payee_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// record is one row of the JSONL output — emitted PER ATTEMPT (not per intent).
type record struct {
	TS             string `json:"ts"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	Payer          string `json:"payer"`
	Payee          string `json:"payee"`
	Amount         int64  `json:"amount"`
	Status         int    `json:"status"`
	LatencyMs      int64  `json:"latency_ms"`
	Retried        bool   `json:"retried,omitempty"`  // true if another attempt follows this one
	Final          bool   `json:"final,omitempty"`    // true if this is the last attempt for this intent
	Replayed       bool   `json:"replayed,omitempty"` // true if server returned Idempotency-Replay: true
	Error          string `json:"error,omitempty"`
}

// isRetryable returns true if this status/error warrants another attempt.
// 4xx (except 408 + 429) are client errors — no amount of retry helps.
func isRetryable(status int, err error) bool {
	if err != nil {
		return true // connection error, context timeout, etc.
	}
	if status == 0 {
		return true // no response received
	}
	if status >= 500 {
		return true // transient server error
	}
	if status == 408 || status == 429 {
		return true // timeout / rate-limited
	}
	return false
}

func main() {
	target            := flag.String("target", "http://localhost:8080", "payment-api URL")
	rps               := flag.Int("rps", 100, "target intents per second")
	workers           := flag.Int("workers", 10, "concurrent worker count")
	duration          := flag.Duration("duration", 60*time.Second, "run duration")
	seed              := flag.Int64("seed", time.Now().UnixNano(), "random seed")
	accounts          := flag.Int("accounts", 100, "account pool size")
	output            := flag.String("output", "", "JSONL output path (optional)")
	retries           := flag.Int("retries", 3, "max attempts per intent")
	backoffBaseMs     := flag.Int("backoff-base-ms", 200, "exponential backoff base (ms)")
	perAttemptTimeout := flag.Duration("per-attempt-timeout", 5*time.Second, "per-attempt HTTP timeout")
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

	// httpClient timeout is outer safety net — perAttemptTimeout (5s) fires first.
	httpClient := &http.Client{Timeout: 10 * time.Second}

	ticker := time.NewTicker(time.Second / time.Duration(*rps))
	defer ticker.Stop()

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
		// per-request counters (backward compat with v1 summary)
		requestsTotal int64
		completed2xx  int64
		rejected4xx   int64
		failed5xx     int64
		// per-intent counters (v2)
		intentsSent      int64
		intentsCompleted int64
		intentsFailed    int64
		replaysServed    int64
		// latency (successful requests only)
		latenciesMu sync.Mutex
		latencies   []int64
	)

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerIndex int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(workerIndex)))

			for range jobs {
				// Pick a unique payer/payee pair + amount for this intent.
				p1 := accountIDs[rng.Intn(len(accountIDs))]
				p2 := accountIDs[rng.Intn(len(accountIDs))]
				for p1 == p2 {
					p2 = accountIDs[rng.Intn(len(accountIDs))]
				}
				amount := int64(rng.Intn(5000) + 1)
				body, _ := json.Marshal(transferReq{
					PayerID: p1, PayeeID: p2, AmountMinor: amount, Currency: "USD",
				})

				// One idempotency key per intent — reused across retries.
				intentKey := uuid.NewString()
				atomic.AddInt64(&intentsSent, 1)

				for attempt := 1; attempt <= *retries; attempt++ {
					reqBody := bytes.NewReader(body)
					attemptCtx, cancel := context.WithTimeout(ctx, *perAttemptTimeout)

					req, _ := http.NewRequestWithContext(attemptCtx, "POST",
						*target+"/v1/transfer", reqBody)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Idempotency-Key", intentKey)

					reqStart := time.Now()
					resp, err := httpClient.Do(req)
					latency := time.Since(reqStart).Milliseconds()
					cancel()

					rec := record{
						TS:             time.Now().UTC().Format(time.RFC3339Nano),
						IdempotencyKey: intentKey,
						Attempt:        attempt,
						Payer:          p1,
						Payee:          p2,
						Amount:         amount,
						LatencyMs:      latency,
					}

					if err != nil {
						rec.Status = 0
						rec.Error = err.Error()
						atomic.AddInt64(&failed5xx, 1)
					} else {
						rec.Status = resp.StatusCode
						if resp.Header.Get("Idempotency-Replay") == "true" {
							rec.Replayed = true
							atomic.AddInt64(&replaysServed, 1)
						}
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
					atomic.AddInt64(&requestsTotal, 1)

					needRetry := isRetryable(rec.Status, err) && attempt < *retries
					rec.Retried = needRetry
					rec.Final = !needRetry

					select {
					case records <- rec:
					case <-ctx.Done():
					}

					latenciesMu.Lock()
					latencies = append(latencies, latency)
					latenciesMu.Unlock()

					if !needRetry {
						if rec.Status >= 200 && rec.Status < 300 {
							atomic.AddInt64(&intentsCompleted, 1)
						} else {
							atomic.AddInt64(&intentsFailed, 1)
						}
						break
					}

					// Exponential backoff with full jitter.
					sleep := time.Duration(*backoffBaseMs) * time.Millisecond * (1 << (attempt - 1))
					if sleep > 0 {
						jitter := time.Duration(rng.Int63n(int64(sleep / 2)))
						time.Sleep(sleep + jitter)
					}
				}
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
		"event":             "simulator.summary",
		// v2 intent-level counters
		"intents_sent":      atomic.LoadInt64(&intentsSent),
		"intents_completed": atomic.LoadInt64(&intentsCompleted),
		"intents_failed":    atomic.LoadInt64(&intentsFailed),
		"replays_served":    atomic.LoadInt64(&replaysServed),
		// request-level counters (includes retries)
		"requests_total":    atomic.LoadInt64(&requestsTotal),
		"completed_2xx":     atomic.LoadInt64(&completed2xx),
		"rejected_4xx":      atomic.LoadInt64(&rejected4xx),
		"failed_5xx":        atomic.LoadInt64(&failed5xx),
		"p50_ms":            p50,
		"p95_ms":            p95,
		"p99_ms":            p99,
		"duration_s":        totalDuration.Seconds(),
		"actual_intents_rps": float64(atomic.LoadInt64(&intentsSent)) / totalDuration.Seconds(),
		"seed":              *seed,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
}

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
