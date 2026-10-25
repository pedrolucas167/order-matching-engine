package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newDurableFuturesTest(t *testing.T) (*DurableMatchingEngine, string, uuid.UUID, uuid.UUID) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	engine, err := NewDurableDerivativesMatchingEngine(path, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := engine.AddClient(&Client{ID: id, Collateral: 1000, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	return engine, path, buyer, seller
}

func durableSubmit(t *testing.T, engine *DurableMatchingEngine, key string, order *Order) OrderReceipt {
	t.Helper()
	receipt, _, err := engine.SubmitOrderRequest(context.Background(), key, order)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func reopenDurableTest(t *testing.T, path string) *DurableMatchingEngine {
	t.Helper()
	engine, err := NewDurableDerivativesMatchingEngine(path, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func TestDurableRecoveryPreservesReceiptsFIFOPositionsAndOutbox(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	first, second := derivativeTestOrder(seller, Ask, 2), derivativeTestOrder(seller, Ask, 1)
	first.Price = 100
	second.Price = 100
	durableSubmit(t, engine, "first", first)
	durableSubmit(t, engine, "second", second)
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Price = 100
	original := durableSubmit(t, engine, "taker", bid)
	before, err := engine.GetMarginAccount(seller)
	if err != nil {
		t.Fatal(err)
	}
	events, err := engine.ReadTrades(0, 10)
	if err != nil || len(events) != 1 || events[0].Trade.AskOrder != first.ID {
		t.Fatalf("FIFO execution not retained: %+v %v", events, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := reopenDurableTest(t, path)
	after, err := recovered.GetMarginAccount(seller)
	if err != nil || after != before {
		t.Fatalf("financial state changed across restart: %+v %+v %v", before, after, err)
	}
	receipt, replayed, err := recovered.SubmitOrderRequest(context.Background(), "taker", bid)
	if err != nil || !replayed || receipt != original {
		t.Fatal("restart lost original receipt", err)
	}
	snapshot, err := recovered.GetSnapshot("SIM-FUT")
	if err != nil || snapshot.BestAsk.TotalQty != 2 || snapshot.BestAsk.Orders[0].ID != first.ID || snapshot.BestAsk.Orders[1].ID != second.ID {
		t.Fatalf("resting FIFO changed: %+v %v", snapshot, err)
	}
	republished, err := recovered.ReadTrades(0, 10)
	if err != nil || len(republished) != 1 || republished[0] != events[0] {
		t.Fatal("unacknowledged trade changed on replay", err)
	}
	if err := recovered.AcknowledgeTrades(events[0].Sequence); err != nil {
		t.Fatal(err)
	}
	if err := recovered.AcknowledgeTrades(events[0].Sequence); err != nil {
		t.Fatal("duplicate acknowledgment failed", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := reopenDurableTest(t, path)
	remaining, err := restarted.ReadTrades(1, 10)
	if err != nil || len(remaining) != 0 {
		t.Fatal("acknowledged trade returned after restart", err)
	}
	if _, err := restarted.ReadTrades(0, 10); err == nil {
		t.Fatal("discarded cursor was accepted silently")
	}
	if _, err := restarted.GetMarginAccount(uuid.New()); !errors.Is(err, ErrClientNotFound) {
		t.Fatal(err)
	}
	if _, exists, err := restarted.GetDerivativePosition(buyer, "SIM-FUT"); err != nil || !exists {
		t.Fatal(err)
	}
	next := derivativeTestOrder(buyer, Bid, 1)
	next.Price = 100
	durableSubmit(t, restarted, "next", next)
	newEvents, err := restarted.ReadTrades(1, 10)
	if err != nil || len(newEvents) != 1 || newEvents[0].Sequence != 2 || newEvents[0].Trade.AskOrder != first.ID {
		t.Fatal("recovery lost event sequence or maker priority", err)
	}
}

func TestDurableCancellationStopsAndFundingSurviveRestart(t *testing.T) {
	engine, path, buyer, _ := newDurableFuturesTest(t)
	limit := derivativeTestOrder(buyer, Bid, 1)
	durableSubmit(t, engine, "limit", limit)
	stop := derivativeTestOrder(buyer, Bid, 1)
	stop.Type = StopLimit
	stop.StopPrice = 20000
	original := durableSubmit(t, engine, "stop", stop)
	if canceled, err := engine.CancelOrder(limit.ID, limit.Symbol); err != nil || !canceled {
		t.Fatal(err)
	}
	if err := engine.AddClient(&Client{ID: buyer, Collateral: 1500, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	account, err := recovered.GetMarginAccount(buyer)
	if err != nil || account.Collateral != 1500 || account.OrderMargin != 100 {
		t.Fatalf("funding or reservation lost: %+v %v", account, err)
	}
	if canceled, err := recovered.CancelOrder(limit.ID, limit.Symbol); err != nil || canceled {
		t.Fatal("canceled limit returned", err)
	}
	if receipt, replayed, err := recovered.SubmitOrderRequest(context.Background(), "stop", stop); err != nil || !replayed || receipt != original {
		t.Fatal("pending stop lost receipt", err)
	}
	if canceled, err := recovered.CancelStopOrder(stop.ID, stop.Symbol); err != nil || !canceled {
		t.Fatal("pending stop could not cancel", err)
	}
	recovered.Close()
	last := reopenDurableTest(t, path)
	account, err = last.GetMarginAccount(buyer)
	if err != nil || account.OrderMargin != 0 {
		t.Fatal("stop cancellation was not durable", err)
	}
	if _, replay, err := last.SubmitOrderRequest(context.Background(), "stop", stop); err != nil || !replay {
		t.Fatal("canceled stop was resubmitted", err)
	}
}

func TestDurableWriteFailureFencesAndResolvesAmbiguousCommit(t *testing.T) {
	for _, persistBeforeError := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[persistBeforeError], func(t *testing.T) {
			engine, path, buyer, seller := newDurableFuturesTest(t)
			ask := derivativeTestOrder(seller, Ask, 1)
			ask.Price = 100
			durableSubmit(t, engine, "maker", ask)
			engine.mu.Lock()
			write := engine.writeState
			engine.writeState = func(payload []byte) error {
				if persistBeforeError {
					if err := write(payload); err != nil {
						return err
					}
				}
				return io.ErrClosedPipe
			}
			engine.mu.Unlock()
			bid := derivativeTestOrder(buyer, Bid, 1)
			bid.Price = 100
			if _, _, err := engine.SubmitOrderRequest(context.Background(), "uncertain", bid); !errors.Is(err, ErrRequestOutcomeUnknown) || !errors.Is(err, ErrDurableStorage) {
				t.Fatal("write failure acknowledged success", err)
			}
			if _, err := engine.GetMarginAccount(buyer); !errors.Is(err, ErrDurableStorage) {
				t.Fatal("uncommitted state remained visible", err)
			}
			if _, err := engine.ReadTrades(0, 10); !errors.Is(err, ErrDurableStorage) {
				t.Fatal("uncommitted trade became visible", err)
			}
			if err := engine.AddClient(&Client{ID: buyer, Collateral: 2000, MarginCurrency: "SIM"}); !errors.Is(err, ErrDurableStorage) {
				t.Fatal("failed engine accepted more mutations", err)
			}
			engine.Close()
			recovered := reopenDurableTest(t, path)
			receipt, replayed, err := recovered.SubmitOrderRequest(context.Background(), "uncertain", bid)
			if err != nil || replayed != persistBeforeError || receipt.FilledQuantity != 1 {
				t.Fatalf("uncertain commit not reconciled: %+v %v %v", receipt, replayed, err)
			}
			stats, err := recovered.GetStats()
			if err != nil || stats.TradesProcessed != 1 || stats.OrdersProcessed != 2 {
				t.Fatalf("failure replay duplicated financial effects: %+v %v", stats, err)
			}
		})
	}
}

func TestDurableReceiptIsNotAcknowledgedBeforeStorageCommit(t *testing.T) {
	engine, _, buyer, _ := newDurableFuturesTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	engine.mu.Lock()
	write := engine.writeState
	engine.writeState = func(payload []byte) error { close(entered); <-release; return write(payload) }
	engine.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.SubmitOrderRequest(ctx, "delayed", derivativeTestOrder(buyer, Bid, 1))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("write did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before commit resolved: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, ErrRequestOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled acknowledgment not reported", err)
	}
}

func TestDurableDatabaseRejectsConcurrentOwner(t *testing.T) {
	engine, path, _, _ := newDurableFuturesTest(t)
	if other, err := NewDurableDerivativesMatchingEngine(path, 4, 4); err == nil {
		other.Close()
		t.Fatal("second writer acquired the same database")
	}
	engine.Close()
	reopenDurableTest(t, path)
}
