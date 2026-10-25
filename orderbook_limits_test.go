package main

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestDerivativePriceLevelOverflowRejectsAndReleasesRemainder(t *testing.T) {
	engine := NewDerivativesMatchingEngine(16, 16)
	defer engine.Close()
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 1}); err != nil {
		t.Fatal(err)
	}
	clients := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, id := range clients {
		if err := engine.GetRiskManager().AddClient(&Client{ID: id, Collateral: math.MaxInt64, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range clients[:2] {
		submitTestOrder(t, engine, derivativeTestOrder(id, Ask, math.MaxInt64))
	}
	rejected := derivativeTestOrder(clients[2], Ask, 2)
	submitTestOrder(t, engine, rejected)
	snapshot := engine.GetSnapshot("SIM-FUT")
	if snapshot.BestAsk.TotalQty != math.MaxUint64-1 || len(snapshot.BestAsk.Orders) != 2 {
		t.Fatalf("price level quantity overflowed: total=%d orders=%d", snapshot.BestAsk.TotalQty, len(snapshot.BestAsk.Orders))
	}
	account, _ := engine.GetRiskManager().GetMarginAccount(clients[2])
	if account.OrderMargin != 0 || account.AvailableCollateral != math.MaxInt64 {
		t.Fatalf("unrested quantity retained margin: %+v", account)
	}
	if _, exists := engine.GetOrderBook("SIM-FUT").GetOrder(rejected.ID); exists {
		t.Fatal("overflowing order reached the index")
	}
}

func TestOrderBookRejectsOverflowWithoutChangingIndex(t *testing.T) {
	book := NewOrderBook("TEST")
	first := testOrder(Bid, Limit, math.MaxUint64)
	second := testOrder(Bid, Limit, 1)
	book.AddOrder(first)
	book.AddOrder(second)
	if snapshot := book.GetSnapshot(); snapshot.BestBid.TotalQty != math.MaxUint64 || len(snapshot.BestBid.Orders) != 1 {
		t.Fatal("standalone book quantity overflowed")
	}
	if _, exists := book.GetOrder(second.ID); exists {
		t.Fatal("rejected order reached index")
	}
}
