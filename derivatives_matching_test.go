package main

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func newDerivativeEngineTest(t *testing.T, collateral uint64) (*MatchingEngine, uuid.UUID, uuid.UUID) {
	t.Helper()
	engine := NewDerivativesMatchingEngine(512, 512)
	t.Cleanup(engine.Close)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := engine.GetRiskManager().AddClient(&Client{ID: id, Collateral: collateral, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	return engine, buyer, seller
}

func assertDerivativeAccount(t *testing.T, manager *RiskManager, clientID uuid.UUID, positionMargin, orderMargin, available uint64) {
	t.Helper()
	account, exists := manager.GetMarginAccount(clientID)
	if !exists || account.PositionMargin != positionMargin || account.OrderMargin != orderMargin || account.AvailableCollateral != available {
		t.Fatalf("unexpected margin account: %+v exists=%v", account, exists)
	}
}

func TestDerivativeEnginePartialFillCancelAndClosePosition(t *testing.T) {
	engine, buyer, seller := newDerivativeEngineTest(t, 500)
	manager := engine.GetRiskManager()
	bid := derivativeTestOrder(buyer, Bid, 3)
	submitTestOrder(t, engine, bid)
	synchronizeEngine(t, engine)
	assertDerivativeAccount(t, manager, buyer, 0, 300, 200)
	ask := derivativeTestOrder(seller, Ask, 1)
	ask.Type = Market
	ask.Price = 0
	submitTestOrder(t, engine, ask)
	if stats := engine.GetStats(); stats.TradesProcessed != 1 {
		t.Fatalf("derivative sale did not match: %+v", stats)
	}
	trade := readTestTrade(t, engine)
	if trade.Quantity != 1 || trade.Price != 10000 {
		t.Fatalf("unexpected derivative trade: %+v", trade)
	}
	assertDerivativeAccount(t, manager, buyer, 100, 200, 200)
	assertDerivativeAccount(t, manager, seller, 100, 0, 400)
	short, _ := manager.GetDerivativePosition(seller, "SIM-FUT")
	if short.NetContracts != -1 {
		t.Fatalf("sale did not open a short: %+v", short)
	}
	if !engine.CancelOrder(bid.ID, bid.Symbol) {
		t.Fatal("partial derivative order could not be cancelled")
	}
	assertDerivativeAccount(t, manager, buyer, 100, 0, 400)
	closeAsk := derivativeTestOrder(buyer, Ask, 1)
	closeBid := derivativeTestOrder(seller, Bid, 1)
	closeBid.Type = Market
	closeBid.Price = 0
	submitTestOrder(t, engine, closeAsk)
	submitTestOrder(t, engine, closeBid)
	if stats := engine.GetStats(); stats.TradesProcessed != 2 {
		t.Fatal("closing trade did not execute")
	}
	readTestTrade(t, engine)
	for _, id := range []uuid.UUID{buyer, seller} {
		position, _ := manager.GetDerivativePosition(id, "SIM-FUT")
		if position != (DerivativePosition{}) {
			t.Fatalf("position remained after closing: %+v", position)
		}
		assertDerivativeAccount(t, manager, id, 0, 0, 500)
	}
}

func TestDerivativeEngineConcurrentAdmissionCannotReuseMargin(t *testing.T) {
	engine, buyer, _ := newDerivativeEngineTest(t, 100)
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := engine.SubmitOrder(derivativeTestOrder(buyer, Bid, 1)); err != nil {
				t.Error(err)
			}
			engine.GetRiskManager().GetMarginAccount(buyer)
		}()
	}
	workers.Wait()
	if stats := engine.GetStats(); stats.OrdersProcessed != 64 || stats.TradesProcessed != 0 {
		t.Fatalf("unexpected admission counters: %+v", stats)
	}
	snapshot := engine.GetSnapshot("SIM-FUT")
	if snapshot.BestBid == nil || len(snapshot.BestBid.Orders) != 1 || snapshot.BestBid.TotalQty != 1 {
		t.Fatal("multiple orders reused the same collateral")
	}
	assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 0, 100, 0)
	if rejected := engine.GetRiskManager().GetStats().OrdersRejected; rejected != 63 {
		t.Fatalf("margin rejections=%d; want 63", rejected)
	}
	if !engine.CancelOrder(snapshot.BestBid.Orders[0].ID, "SIM-FUT") {
		t.Fatal("funded order was not cancellable")
	}
	assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 0, 0, 100)
	replacement := derivativeTestOrder(buyer, Bid, 1)
	submitTestOrder(t, engine, replacement)
	synchronizeEngine(t, engine)
	if _, exists := engine.GetOrderBook("SIM-FUT").GetOrder(replacement.ID); !exists {
		t.Fatal("released margin could not be reused")
	}
}

func TestDerivativeEngineMarketRemainderReleasesOnlyOrderMargin(t *testing.T) {
	engine, buyer, seller := newDerivativeEngineTest(t, 200)
	submitTestOrder(t, engine, derivativeTestOrder(seller, Ask, 1))
	market := derivativeTestOrder(buyer, Bid, 2)
	market.Type = Market
	market.Price = 0
	submitTestOrder(t, engine, market)
	if stats := engine.GetStats(); stats.TradesProcessed != 1 {
		t.Fatal("partially filled market order was not executed")
	}
	readTestTrade(t, engine)
	assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 100, 0, 100)
	position, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "SIM-FUT")
	if position.NetContracts != 1 || position.OpenBuyContracts != 0 {
		t.Fatalf("unfilled market remainder remained reserved: %+v", position)
	}
}

func TestDerivativeEngineStopsReserveAndReleaseMargin(t *testing.T) {
	for _, kind := range []OrderType{StopMarket, StopLimit} {
		name := map[OrderType]string{StopMarket: "market", StopLimit: "limit"}[kind]
		t.Run(name, func(t *testing.T) {
			engine, buyer, seller := newDerivativeEngineTest(t, 300)
			manager := engine.GetRiskManager()
			cancelled := derivativeTestOrder(buyer, Bid, 2)
			cancelled.Type = kind
			cancelled.StopPrice = 200
			cancelled.Price = 200
			submitTestOrder(t, engine, cancelled)
			synchronizeEngine(t, engine)
			assertDerivativeAccount(t, manager, buyer, 0, 200, 100)
			if !engine.CancelStopOrder(cancelled.ID, "SIM-FUT") {
				t.Fatal("pending stop was not cancellable")
			}
			assertDerivativeAccount(t, manager, buyer, 0, 0, 300)
			stop := derivativeTestOrder(buyer, Bid, 2)
			stop.Type = kind
			stop.StopPrice = 200
			stop.Price = 200
			submitTestOrder(t, engine, stop)
			ask := derivativeTestOrder(seller, Ask, 1)
			ask.Price = 200
			submitTestOrder(t, engine, ask)
			engine.runCommand(func() { engine.checkAndTriggerStopOrders(); engine.checkAndTriggerStopOrders() })
			if stats := engine.GetStats(); stats.TradesProcessed != 1 || stats.PendingStopOrders != 0 {
				t.Fatalf("unexpected triggered stop: %+v", stats)
			}
			trade := readTestTrade(t, engine)
			if trade.BidOrder != stop.ID || trade.BidClient != buyer {
				t.Fatal("stop lost its reserved identity")
			}
			if kind == StopLimit {
				assertDerivativeAccount(t, manager, buyer, 100, 100, 100)
				if !engine.CancelOrder(stop.ID, "SIM-FUT") {
					t.Fatal("triggered stop-limit remainder was not cancellable")
				}
			}
			assertDerivativeAccount(t, manager, buyer, 100, 0, 200)
		})
	}
}

func TestDerivativeEngineCloseReleasesOrdersButKeepsPositions(t *testing.T) {
	engine, buyer, seller := newDerivativeEngineTest(t, 300)
	manager := engine.GetRiskManager()
	submitTestOrder(t, engine, derivativeTestOrder(seller, Ask, 1))
	submitTestOrder(t, engine, derivativeTestOrder(buyer, Bid, 1))
	synchronizeEngine(t, engine)
	readTestTrade(t, engine)
	pending := derivativeTestOrder(buyer, Bid, 1)
	pending.Price = 9000
	stop := derivativeTestOrder(buyer, Bid, 1)
	stop.Type = StopMarket
	stop.StopPrice = 20000
	submitTestOrder(t, engine, pending)
	submitTestOrder(t, engine, stop)
	synchronizeEngine(t, engine)
	assertDerivativeAccount(t, manager, buyer, 100, 200, 0)
	engine.Close()
	assertDerivativeAccount(t, manager, buyer, 100, 0, 200)
	position, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	if position.NetContracts != 1 || position.OpenBuyContracts != 0 {
		t.Fatalf("shutdown changed settled position: %+v", position)
	}
}

func TestDerivativeEnginePositionsRemainSeparateByInstrument(t *testing.T) {
	engine, buyer, seller := newDerivativeEngineTest(t, 500)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "OTHER-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 150}); err != nil {
		t.Fatal(err)
	}
	submitTestOrder(t, engine, derivativeTestOrder(seller, Ask, 1))
	submitTestOrder(t, engine, derivativeTestOrder(buyer, Bid, 1))
	otherAsk, otherBid := derivativeTestOrder(buyer, Ask, 1), derivativeTestOrder(seller, Bid, 1)
	otherAsk.Symbol = "OTHER-FUT"
	otherBid.Symbol = "OTHER-FUT"
	submitTestOrder(t, engine, otherAsk)
	submitTestOrder(t, engine, otherBid)
	if stats := engine.GetStats(); stats.TradesProcessed != 2 {
		t.Fatal("multi-instrument trades did not execute")
	}
	first, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "SIM-FUT")
	second, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "OTHER-FUT")
	if first.NetContracts != 1 || second.NetContracts != -1 {
		t.Fatalf("positions were mixed: %+v %+v", first, second)
	}
	assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 250, 0, 250)
}

func TestDerivativeEngineRejectsSelfTradesWithoutChangingMaker(t *testing.T) {
	engine, buyer, _ := newDerivativeEngineTest(t, 200)
	ask := derivativeTestOrder(buyer, Ask, 1)
	submitTestOrder(t, engine, ask)
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Type = Market
	bid.Price = 0
	submitTestOrder(t, engine, bid)
	if stats := engine.GetStats(); stats.TradesProcessed != 0 {
		t.Fatal("self-trade was published")
	}
	snapshot := engine.GetSnapshot("SIM-FUT")
	if snapshot.BestAsk == nil || snapshot.BestAsk.Orders[0].ID != ask.ID || snapshot.BestAsk.TotalQty != 1 {
		t.Fatal("self-trade changed the maker")
	}
	position, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "SIM-FUT")
	if position.NetContracts != 0 || position.OpenBuyContracts != 0 || position.OpenSellContracts != 1 {
		t.Fatalf("self-trade changed positions: %+v", position)
	}
	assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 0, 100, 100)
}

func TestDerivativeEngineRejectsUnregisteredClientsAndInstruments(t *testing.T) {
	engine, _, _ := newDerivativeEngineTest(t, 200)
	unknown := derivativeTestOrder(uuid.New(), Bid, 1)
	submitTestOrder(t, engine, unknown)
	unknownSymbol := derivativeTestOrder(uuid.New(), Ask, 1)
	unknownSymbol.Symbol = "UNKNOWN"
	submitTestOrder(t, engine, unknownSymbol)
	if stats := engine.GetStats(); stats.ActiveBooks != 0 || stats.TradesProcessed != 0 || stats.PendingStopOrders != 0 {
		t.Fatalf("unfunded orders reached matching: %+v", stats)
	}
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "BAD", MarginCurrency: "SIM"}); err == nil {
		t.Fatal("zero margin instrument accepted")
	}
	legacy := NewMatchingEngine(1, 1)
	defer legacy.Close()
	if err := legacy.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100}); err == nil {
		t.Fatal("legacy simulation silently changed financial mode")
	}
}

func TestDuplicateDerivativeOrderCannotReleaseOriginalMargin(t *testing.T) {
	for _, kind := range []OrderType{Limit, StopLimit} {
		name := map[OrderType]string{Limit: "resting", StopLimit: "pendingStop"}[kind]
		t.Run(name, func(t *testing.T) {
			engine, buyer, _ := newDerivativeEngineTest(t, 200)
			original := derivativeTestOrder(buyer, Bid, 2)
			original.Type = kind
			original.StopPrice = 20000
			submitTestOrder(t, engine, original)
			duplicate := derivativeTestOrder(buyer, Ask, 1)
			duplicate.ID = original.ID
			duplicate.Type = Market
			duplicate.Price = 0
			submitTestOrder(t, engine, duplicate)
			if stats := engine.GetStats(); stats.TradesProcessed != 0 {
				t.Fatal("duplicate derivative order executed")
			}
			assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 0, 200, 0)
			if kind == Limit {
				if !engine.CancelOrder(original.ID, "SIM-FUT") {
					t.Fatal("original resting order was lost")
				}
			} else if !engine.CancelStopOrder(original.ID, "SIM-FUT") {
				t.Fatal("original pending stop was lost")
			}
			assertDerivativeAccount(t, engine.GetRiskManager(), buyer, 0, 0, 200)
		})
	}
}
