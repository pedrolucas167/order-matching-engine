package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testOrder(side Side, kind OrderType, quantity uint64) *Order {
	return &Order{ID: uuid.New(), ClientID: uuid.New(), Symbol: "TEST", Side: side, Type: kind, Price: 100, StopPrice: 100, Quantity: quantity, Remaining: quantity}
}

func submitTestOrder(t *testing.T, engine *MatchingEngine, order *Order) {
	t.Helper()
	if err := engine.SubmitOrder(order); err != nil {
		t.Fatal(err)
	}
}

func synchronizeEngine(t *testing.T, engine *MatchingEngine) {
	t.Helper()
	if !engine.runCommand(func() {}) {
		t.Fatal("engine closed unexpectedly")
	}
}

func TestInputAndReadIsolation(t *testing.T) {
	engine := NewMatchingEngine(32, 32)
	defer engine.Close()
	order := testOrder(Ask, Limit, 10)
	id := order.ID
	submitTestOrder(t, engine, order)
	order.Remaining = 999
	order.Price = 999
	synchronizeEngine(t, engine)
	snapshot := engine.GetSnapshot("TEST")
	if snapshot.BestAsk.Orders[0].Remaining != 10 {
		t.Fatal("submitted order aliases input")
	}
	snapshot.BestAsk.Orders[0].Remaining = 800
	snapshot.BestAsk.Price = 800
	detached := engine.GetOrderBook("TEST")
	detached.RemoveOrder(id)
	fresh := engine.GetSnapshot("TEST")
	if fresh.BestAsk.Price != 100 || fresh.BestAsk.Orders[0].Remaining != 10 {
		t.Fatal("read exposed engine state")
	}
	submitTestOrder(t, engine, testOrder(Bid, Market, 4))
	synchronizeEngine(t, engine)
	fresh = engine.GetSnapshot("TEST")
	if fresh.BestAsk.Orders[0].Remaining != 6 || fresh.BestAsk.TotalQty != 6 {
		t.Fatalf("partial fill snapshot: %+v", fresh.BestAsk)
	}
	if snapshot.BestAsk.Orders[0].Remaining != 800 {
		t.Fatal("old snapshot changed after matching")
	}
}

func TestCancellationUsesCommandOrder(t *testing.T) {
	engine := NewMatchingEngine(32, 32)
	defer engine.Close()
	order := testOrder(Ask, Limit, 1)
	submitTestOrder(t, engine, order)
	if !engine.CancelOrder(order.ID, order.Symbol) {
		t.Fatal("cancel must see preceding submission")
	}
	if engine.CancelOrder(order.ID, order.Symbol) {
		t.Fatal("cancelled twice")
	}
	filled := testOrder(Ask, Limit, 1)
	submitTestOrder(t, engine, filled)
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	if engine.CancelOrder(filled.ID, filled.Symbol) {
		t.Fatal("cancel succeeded after fill")
	}
	stop := testOrder(Bid, StopMarket, 1)
	stop.StopPrice = 200
	submitTestOrder(t, engine, stop)
	if !engine.CancelStopOrder(stop.ID, stop.Symbol) || engine.CancelStopOrder(stop.ID, stop.Symbol) {
		t.Fatal("pending stop cancellation inconsistent")
	}
}

func TestConcurrentMatchingStopsCancellationAndReads(t *testing.T) {
	engine := NewMatchingEngine(1024, 1024)
	defer engine.Close()
	const count = 100
	for i := 0; i < count; i++ {
		submitTestOrder(t, engine, testOrder(Ask, Limit, 1))
	}
	synchronizeEngine(t, engine)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := Market
			if i%2 == 0 {
				kind = StopMarket
			}
			order := testOrder(Bid, kind, 1)
			if err := engine.SubmitOrder(order); err != nil {
				t.Error(err)
				return
			}
			if kind == StopMarket {
				engine.CancelStopOrder(order.ID, order.Symbol)
			}
			engine.GetSnapshot("TEST")
			engine.GetOrderBook("TEST")
			engine.GetStats()
		}(i)
	}
	wg.Wait()
	engine.runCommand(func() { engine.checkAndTriggerStopOrders(); engine.checkAndTriggerStopOrders() })
	stats := engine.GetStats()
	seenAsks := make(map[uuid.UUID]bool)
	seenBids := make(map[uuid.UUID]bool)
	for i := uint64(0); i < stats.TradesProcessed; i++ {
		select {
		case trade := <-engine.GetTradeChannel():
			if seenAsks[trade.AskOrder] || seenBids[trade.BidOrder] {
				t.Fatal("order executed more than once")
			}
			if trade.BidClient == uuid.Nil {
				t.Fatal("triggered stop lost client identity")
			}
			seenAsks[trade.AskOrder] = true
			seenBids[trade.BidOrder] = true
		case <-time.After(3 * time.Second):
			t.Fatal("missing published trade")
		}
	}
	snapshot := engine.GetSnapshot("TEST")
	var remaining uint64
	if snapshot.BestAsk != nil {
		remaining = snapshot.BestAsk.TotalQty
	}
	if stats.TradesProcessed+remaining != count {
		t.Fatalf("quantity not conserved: trades=%d remaining=%d", stats.TradesProcessed, remaining)
	}
}

func TestSlowTradeConsumerDoesNotBlockCancellationOrClose(t *testing.T) {
	engine := NewMatchingEngine(16, 0)
	resting := testOrder(Ask, Limit, 2)
	submitTestOrder(t, engine, resting)
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	synchronizeEngine(t, engine)
	if err := engine.SubmitOrder(testOrder(Bid, Market, 1)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected trade backlog backpressure, got %v", err)
	}
	completed := make(chan struct{})
	go func() {
		if !engine.CancelOrder(resting.ID, resting.Symbol) {
			t.Error("cancel failed with blocked consumer")
		}
		engine.Close()
		engine.Close()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel or close blocked")
	}
	if err := engine.SubmitOrder(testOrder(Bid, Limit, 1)); err == nil {
		t.Fatal("accepted after shutdown")
	}
	if engine.CancelOrder(resting.ID, resting.Symbol) {
		t.Fatal("cancel accepted after shutdown")
	}
}

func TestOrderBookCopiesPublicValues(t *testing.T) {
	book := NewOrderBook("TEST")
	order := testOrder(Ask, Limit, 10)
	book.AddOrder(order)
	order.Remaining = 50
	best := book.GetBestAsk()
	best.Orders[0].Remaining = 100
	read, _ := book.GetOrder(order.ID)
	read.Remaining = 200
	snapshot := book.GetSnapshot()
	snapshot.BestAsk.Orders[0].Remaining = 300
	if got, _ := book.GetOrder(order.ID); got.Remaining != 10 {
		t.Fatal("public book API aliases state")
	}
}

func TestStopsExecuteOnceAndPreserveFIFO(t *testing.T) {
	engine := NewMatchingEngine(32, 32)
	defer engine.Close()
	first := testOrder(Ask, Limit, 1)
	second := testOrder(Ask, Limit, 1)
	submitTestOrder(t, engine, first)
	submitTestOrder(t, engine, second)
	stop := testOrder(Bid, StopMarket, 2)
	submitTestOrder(t, engine, stop)
	engine.runCommand(func() { engine.checkAndTriggerStopOrders(); engine.checkAndTriggerStopOrders() })
	if engine.CancelStopOrder(stop.ID, stop.Symbol) {
		t.Fatal("executed stop remained pending")
	}
	if got := engine.GetStats(); got.TradesProcessed != 2 || got.PendingStopOrders != 0 {
		t.Fatalf("unexpected stats %+v", got)
	}
	for _, expected := range []uuid.UUID{first.ID, second.ID} {
		select {
		case trade := <-engine.GetTradeChannel():
			if trade.AskOrder != expected || trade.BidOrder != stop.ID || trade.BidClient != stop.ClientID {
				t.Fatalf("unexpected stop execution %+v", trade)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing stop execution")
		}
	}
	if engine.GetSnapshot("TEST").BestAsk != nil {
		t.Fatal("filled liquidity remained")
	}
}

func TestShutdownWithConcurrentCommands(t *testing.T) {
	engine := NewMatchingEngine(32, 32)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			engine.SubmitOrder(testOrder(Bid, Limit, 1))
			engine.GetSnapshot("TEST")
			engine.GetStats()
			engine.CancelOrder(uuid.New(), "TEST")
		}()
	}
	engine.Close()
	wg.Wait()
}
func TestTriggeredStopLimitCanBeCancelledAsRestingOrder(t *testing.T) {
	engine := NewMatchingEngine(16, 16)
	defer engine.Close()
	submitTestOrder(t, engine, testOrder(Ask, Limit, 1))
	stop := testOrder(Bid, StopLimit, 1)
	stop.Price = 90
	submitTestOrder(t, engine, stop)
	engine.runCommand(func() { engine.checkAndTriggerStopOrders() })
	if engine.CancelStopOrder(stop.ID, stop.Symbol) {
		t.Fatal("triggered stop still pending")
	}
	if !engine.CancelOrder(stop.ID, stop.Symbol) {
		t.Fatal("triggered limit order could not be cancelled")
	}
}
