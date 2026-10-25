package matching

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConcurrentRequestReplayExecutesOrderOnce(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	ask := derivativeTestOrder(seller, Ask, 1)
	ask.Price = 100
	if receipt, replay, err := engine.SubmitOrderRequest(context.Background(), "maker", ask); err != nil || replay || receipt.InitialStatus != "open" {
		t.Fatalf("maker: %+v %v %v", receipt, replay, err)
	}
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Price = 100
	var workers sync.WaitGroup
	results := make(chan orderRequestResult, 64)
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "taker", bid)
			results <- orderRequestResult{receipt: receipt, replayed: replayed, err: err}
		}()
	}
	workers.Wait()
	close(results)
	var original OrderReceipt
	first, replays := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if original.Sequence == 0 {
			original = result.receipt
		}
		if result.receipt != original || result.receipt.InitialStatus != "filled" || result.receipt.FilledQuantity != 1 {
			t.Fatalf("replay changed receipt: %+v", result)
		}
		if result.replayed {
			replays++
		} else {
			first++
		}
	}
	if first != 1 || replays != 63 || engine.GetStats().TradesProcessed != 1 || engine.GetStats().OrdersProcessed != 2 {
		t.Fatalf("duplicate execution or accounting: first=%d replay=%d stats=%+v", first, replays, engine.GetStats())
	}
	readTestTrade(t, engine)
	position, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "SIM-FUT")
	if position.NetContracts != 1 {
		t.Fatal("retries changed position")
	}
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "taker", bid); err != nil || !replayed {
		t.Fatal("filled order did not replay", err)
	}
}

func TestRequestReplayAfterCancellationAndConflicts(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	order := derivativeTestOrder(buyer, Bid, 1)
	original, _, err := engine.SubmitOrderRequest(context.Background(), "request", order)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.CancelOrder(order.ID, order.Symbol) {
		t.Fatal("order did not cancel")
	}
	receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "request", order)
	if err != nil || !replayed || receipt != original || engine.GetSnapshot(order.Symbol).BestBid != nil {
		t.Fatal("replay recreated canceled order")
	}
	changed := *order
	changed.Quantity = 2
	changed.Remaining = 2
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "request", &changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatal(err)
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "another-key", order); !errors.Is(err, ErrOrderIDConflict) {
		t.Fatal(err)
	}
	submitTestOrder(t, engine, order)
	synchronizeEngine(t, engine)
	if engine.GetSnapshot(order.Symbol).BestBid != nil {
		t.Fatal("legacy entry point bypassed recorded order identity")
	}
}

func TestRejectedRequestRemainsRejectedAfterFundingChanges(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 100, 1)
	order := derivativeTestOrder(buyer, Bid, 2)
	original, _, err := engine.SubmitOrderRequest(context.Background(), "unfunded", order)
	if err != nil || original.InitialStatus != "rejected" || original.RejectionReason == "" {
		t.Fatalf("risk rejection not recorded: %+v %v", original, err)
	}
	if err := engine.GetRiskManager().AddClient(&Client{ID: buyer, Collateral: 1000, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "unfunded", order)
	if err != nil || !replayed || receipt != original || engine.GetStats().OrdersProcessed != 1 {
		t.Fatal("retry reevaluated a terminal risk rejection")
	}
}

func TestRequestCapacityDoesNotEvictReplayGuarantees(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	if err := engine.SetOrderRequestCapacity(1); err != nil {
		t.Fatal(err)
	}
	order := derivativeTestOrder(buyer, Bid, 1)
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "first", order); err != nil {
		t.Fatal(err)
	}
	second := derivativeTestOrder(buyer, Bid, 1)
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "second", second); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal(err)
	}
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "first", order); err != nil || !replayed {
		t.Fatal("full capacity discarded an existing receipt", err)
	}
	if err := engine.SetOrderRequestCapacity(0); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal(err)
	}
	if err := engine.SetOrderRequestCapacity(2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "second", second); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetOrderRequestCapacity(1); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal("capacity shrank below retained records", err)
	}
	engine.Close()
	if err := engine.SetOrderRequestCapacity(2); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
}

func TestTimedOutRequestCanBeRetriedWithoutDuplicateAdmission(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	release := holdMatchingLoop(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	order := derivativeTestOrder(buyer, Bid, 1)
	done := make(chan error, 1)
	go func() { _, _, err := engine.SubmitOrderRequest(ctx, "uncertain", order); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for len(engine.commandChan) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrRequestOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatal("timeout discarded uncertain outcome", err)
	}
	release()
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "uncertain", order); err != nil || !replayed {
		t.Fatal("retry did not find original outcome", err)
	}
	if engine.GetStats().OrdersProcessed != 1 {
		t.Fatal("uncertain request processed twice")
	}
}

func TestRequestValidationAndClosedEngine(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	order := derivativeTestOrder(buyer, Bid, 1)
	for _, key := range []string{"", "has space", "line\nfeed", "á", strings.Repeat("x", 129)} {
		if _, _, err := engine.SubmitOrderRequest(context.Background(), key, order); !errors.Is(err, ErrInvalidOrderRequest) {
			t.Fatal(err)
		}
	}
	if _, _, err := engine.SubmitOrderRequest(nil, "key", order); !errors.Is(err, ErrInvalidOrderRequest) {
		t.Fatal(err)
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "key", nil); !errors.Is(err, ErrInvalidOrder) {
		t.Fatal(err)
	}
	invalid := *order
	invalid.ID = uuid.Nil
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "key", &invalid); !errors.Is(err, ErrInvalidOrder) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := engine.SubmitOrderRequest(ctx, "key", order); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	engine.Close()
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "key", order); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	for status := Pending; status <= Triggered; status++ {
		if orderStatusName(status) == "unknown" {
			t.Fatal("missing status mapping")
		}
	}
	if orderStatusName(-1) != "unknown" || orderStatusName(99) != "unknown" {
		t.Fatal("invalid status mapping")
	}
}

func TestRequestQueueBackpressureDoesNotConsumeKey(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	defer engine.Close()
	release := holdMatchingLoop(t, engine)
	engine.commandChan <- func() {}
	order := testOrder(Bid, Limit, 1)
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "retry", order); !errors.Is(err, ErrBackpressure) {
		t.Fatal(err)
	}
	release()
	synchronizeEngine(t, engine)
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "retry", order); err != nil || replayed {
		t.Fatal("queue rejection consumed key", err)
	}
}

func TestRequestShutdownReportsUncertainQueuedOutcome(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	defer engine.Close()
	release := holdMatchingLoop(t, engine)
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.SubmitOrderRequest(context.Background(), "closing", testOrder(Bid, Limit, 1))
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(engine.commandChan) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request not queued")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { engine.Close(); close(closed) }()
	<-engine.stop
	release()
	<-closed
	if err := <-done; !errors.Is(err, ErrRequestOutcomeUnknown) || !errors.Is(err, ErrEngineClosed) {
		t.Fatal("shutdown lost queued-outcome uncertainty", err)
	}
}

func TestRecordedRequestReplaysWhileTradePublicationIsFull(t *testing.T) {
	engine := NewMatchingEngine(16, 0)
	defer engine.Close()
	ask := testOrder(Ask, Limit, 1)
	first, _, err := engine.SubmitOrderRequest(context.Background(), "maker", ask)
	if err != nil {
		t.Fatal(err)
	}
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	synchronizeEngine(t, engine)
	if receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "maker", ask); err != nil || !replayed || receipt != first {
		t.Fatal("publication backlog blocked retained receipt", err)
	}
	next := testOrder(Bid, Limit, 1)
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "next", next); !errors.Is(err, ErrBackpressure) {
		t.Fatal(err)
	}
	readTestTrade(t, engine)
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "next", next); err != nil || replayed {
		t.Fatal("publication rejection consumed key", err)
	}
}
