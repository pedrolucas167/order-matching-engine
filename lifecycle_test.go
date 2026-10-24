package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func holdMatchingLoop(t *testing.T, engine *MatchingEngine) func() {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseLoop := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseLoop)
	engine.commandChan <- func() { close(started); <-release }
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("matching loop did not start")
	}
	return releaseLoop
}

func TestFullCommandQueueRejectsWithoutChangingTheBook(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	t.Cleanup(engine.Close)
	releaseLoop := holdMatchingLoop(t, engine)
	accepted := testOrder(Ask, Limit, 1)
	rejected := testOrder(Ask, Limit, 5)
	submitTestOrder(t, engine, accepted)
	if err := engine.SubmitOrder(rejected); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("full command queue returned %v", err)
	}
	releaseLoop()
	snapshot := engine.GetSnapshot("TEST")
	if snapshot.BestAsk == nil || len(snapshot.BestAsk.Orders) != 1 || snapshot.BestAsk.Orders[0].ID != accepted.ID || snapshot.BestAsk.TotalQty != 1 {
		t.Fatalf("rejected command changed the book: %+v", snapshot.BestAsk)
	}
	if stats := engine.GetStats(); stats.OrdersProcessed != 1 {
		t.Fatalf("rejected command was counted: %+v", stats)
	}
}

func TestCloseDiscardsQueuedCommandsAndClosesPublicAPIs(t *testing.T) {
	engine := NewMatchingEngine(1, 0)
	t.Cleanup(engine.Close)
	releaseLoop := holdMatchingLoop(t, engine)
	order := testOrder(Ask, Limit, 1)
	submitTestOrder(t, engine, order)
	closed := make(chan struct{})
	go func() { engine.Close(); close(closed) }()
	select {
	case <-engine.stop:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown was not signalled")
	}
	releaseLoop()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if len(engine.books) != 0 {
		t.Fatal("queued command executed after immediate shutdown")
	}
	if err := engine.SubmitOrder(order); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("submission after shutdown returned %v", err)
	}
	if engine.GetSnapshot("TEST") != nil || engine.GetOrderBook("TEST") != nil || engine.GetStats() != (EngineStats{}) {
		t.Fatal("queries returned state after shutdown")
	}
	if engine.CancelOrder(order.ID, "TEST") || engine.CancelStopOrder(order.ID, "TEST") {
		t.Fatal("cancellation succeeded after shutdown")
	}
	select {
	case _, open := <-engine.GetTradeChannel():
		if open {
			t.Fatal("trade channel remained open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("trade channel did not close")
	}
}

func TestTradeBackpressureRecoversAfterConsumerResumes(t *testing.T) {
	engine := NewMatchingEngine(16, 0)
	defer engine.Close()
	resting := testOrder(Ask, Limit, 3)
	submitTestOrder(t, engine, resting)
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	synchronizeEngine(t, engine)
	if err := engine.SubmitOrder(testOrder(Bid, Market, 1)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected trade backpressure; got %v", err)
	}
	first := readTestTrade(t, engine)
	synchronizeEngine(t, engine)
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	second := readTestTrade(t, engine)
	if first.ID == second.ID || first.Quantity != 1 || second.Quantity != 1 {
		t.Fatal("publication duplicated or changed trades")
	}
	if stats := engine.GetStats(); stats.TradesProcessed != 2 {
		t.Fatalf("rejected submission affected trades: %+v", stats)
	}
	snapshot := engine.GetSnapshot("TEST")
	if snapshot.BestAsk.TotalQty != 1 {
		t.Fatalf("remaining quantity=%d; want 1", snapshot.BestAsk.TotalQty)
	}
}
