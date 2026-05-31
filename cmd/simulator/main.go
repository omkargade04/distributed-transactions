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
type record struct {
	TS        string `json:"ts"`
	Payer     string `json:"payer"`
	Payee     string `json:"payee"`
	Amount    int64  `json:"amount"`
	Status    int    `json:"status"`     // HTTP status, or 0 if request errored before reply
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
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
	rps := flag.Int("rps", 100, "target requests per second")
	workers := flag.Int("workers", 10, "concurrent worker count")
	duration := flag.Duration("duration", 60*time.Second, "run duration")
	seed := flag.Int64("seed", time.Now().UnixNano(), "random seed (for reproducible runs)")
	accounts := flag.Int("accounts", 100, "size of pre-seeded account pool")
	output := flag.String("output", "", "JSONL output path (optional)")
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
		sent         int64
		completed2xx int64
		rejected4xx  int64
		failed5xx    int64
		latenciesMu  sync.Mutex
		latencies    []int64
	)

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerIndex int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(workerIndex)))
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
