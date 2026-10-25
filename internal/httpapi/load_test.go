package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func TestHTTPOrderLoad10000RequestsPreservesIdempotencyAndMargin(t *testing.T) {
	const totalRequests, uniqueOrders, workers = 10000, 2500, 64
	engine, buyer, seller := newFuturesPnLTest(t, 1_000_000, 1)
	handler, _ := NewOrderHTTPHandler(engine)
	server := httptest.NewServer(handler)
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: workers, MaxIdleConnsPerHost: workers, MaxConnsPerHost: workers}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	bodies := make([]string, uniqueOrders)
	for i := range bodies {
		id, side := buyer, matching.Bid
		if i%2 == 0 {
			id, side = seller, matching.Ask
		}
		order := derivativeTestOrder(id, side, 1)
		order.Price = 100
		bodies[i] = httpOrderBody(t, order)
	}
	tradeResult := make(chan error, 1)
	go func() {
		seen := make(map[uuid.UUID]bool)
		for len(seen) < uniqueOrders/2 {
			trade, open := <-engine.GetTradeChannel()
			if !open {
				tradeResult <- fmt.Errorf("trade publication closed early")
				return
			}
			if seen[trade.ID] || trade.Quantity != 1 || trade.Price != 100 {
				tradeResult <- fmt.Errorf("duplicate or incorrect trade publication")
				return
			}
			seen[trade.ID] = true
		}
		tradeResult <- nil
	}()
	jobs := make(chan int)
	failures := make(chan string, totalRequests)
	latencies := make([]time.Duration, totalRequests)
	receipts := make([]string, uniqueOrders)
	var receiptMu sync.Mutex
	var originals, replays atomic.Int64
	var group sync.WaitGroup
	started := time.Now()
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for job := range jobs {
				index := job % uniqueOrders
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/orders", strings.NewReader(bodies[index]))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", fmt.Sprintf("load-%d", index))
				requestStarted := time.Now()
				response, err := client.Do(request)
				if err != nil {
					failures <- err.Error()
					continue
				}
				body, readError := io.ReadAll(response.Body)
				response.Body.Close()
				latencies[job] = time.Since(requestStarted)
				if readError != nil || response.StatusCode != http.StatusOK {
					failures <- fmt.Sprintf("HTTP %d: %s", response.StatusCode, body)
					continue
				}
				switch response.Header.Get("Idempotency-Replayed") {
				case "true":
					replays.Add(1)
				case "false":
					originals.Add(1)
				default:
					failures <- "missing replay marker"
				}
				receiptMu.Lock()
				if receipts[index] == "" {
					receipts[index] = string(body)
				} else if receipts[index] != string(body) {
					failures <- "replay changed receipt"
				}
				receiptMu.Unlock()
			}
		}()
	}
	for i := range totalRequests {
		jobs <- i
	}
	close(jobs)
	group.Wait()
	close(failures)
	elapsed := time.Since(started)
	if len(failures) > 0 {
		t.Fatalf("%d failed requests; first: %s", len(failures), <-failures)
	}
	if originals.Load() != uniqueOrders || replays.Load() != totalRequests-uniqueOrders {
		t.Fatalf("incorrect request counts: originals=%d replays=%d", originals.Load(), replays.Load())
	}
	stats := engine.GetStats()
	if stats.OrdersProcessed != uniqueOrders || stats.TradesProcessed != uniqueOrders/2 || stats.PendingStopOrders != 0 {
		t.Fatalf("load duplicated or lost orders: %+v", stats)
	}
	select {
	case err := <-tradeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("trades were not published")
	}
	for _, tc := range []struct {
		id  uuid.UUID
		net int64
	}{{buyer, uniqueOrders / 2}, {seller, -uniqueOrders / 2}} {
		position, _ := engine.GetRiskManager().GetDerivativePosition(tc.id, "SIM-FUT")
		account, _ := engine.GetRiskManager().GetMarginAccount(tc.id)
		if position.NetContracts != tc.net || position.OpenBuyContracts != 0 || position.OpenSellContracts != 0 || account.PositionMargin != 125000 || account.OrderMargin != 0 || account.Equity != 1_000_000 || account.AvailableCollateral != 875000 {
			t.Fatalf("financial invariant failed: %+v %+v", position, account)
		}
	}
	snapshot := engine.GetSnapshot("SIM-FUT")
	if snapshot.BestBid != nil || snapshot.BestAsk != nil {
		t.Fatal("balanced load left residual orders")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("requests=%d unique=%d replays=%d workers=%d elapsed=%s requests_per_second=%.0f p50=%s p95=%s p99=%s go=%s CPUs=%d", totalRequests, uniqueOrders, totalRequests-uniqueOrders, workers, elapsed.Round(time.Millisecond), float64(totalRequests)/elapsed.Seconds(), latencies[(totalRequests-1)*50/100].Round(time.Microsecond), latencies[(totalRequests-1)*95/100].Round(time.Microsecond), latencies[(totalRequests-1)*99/100].Round(time.Microsecond), runtime.Version(), runtime.GOMAXPROCS(0))
}
