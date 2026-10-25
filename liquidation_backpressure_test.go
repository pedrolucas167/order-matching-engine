package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestLiquidationWaitsForTradePublicationCapacity(t *testing.T) {
	engine := NewDerivativesMatchingEngine(64, 0)
	defer engine.Close()
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := engine.GetRiskManager().AddClient(&Client{ID: id, Collateral: 100, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	ask, bid := derivativeTestOrder(seller, Ask, 1), derivativeTestOrder(buyer, Bid, 1)
	ask.Price = 100
	bid.Price = 100
	submitTestOrder(t, engine, ask)
	submitTestOrder(t, engine, bid)
	synchronizeEngine(t, engine)
	enableTestLiquidation(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	status, exists := engine.GetLiquidationStatus(seller, "SIM-FUT")
	position, _ := engine.GetRiskManager().GetDerivativePosition(seller, "SIM-FUT")
	if !exists || status.Reason != "trade publication backlog" || position.NetContracts != -1 || engine.GetStats().TradesProcessed != 1 {
		t.Fatalf("liquidation ignored publication pressure: %+v %+v", status, position)
	}
	trade := readTestTrade(t, engine)
	if trade.Quantity != 1 || trade.Price != 100 {
		t.Fatal("backpressure changed previously executed trade")
	}
}

func TestLiquidationEvaluatesMultipleDistressedAccounts(t *testing.T) {
	engine, buyer, first := newFuturesPnLTest(t, 1000, 1)
	manager := engine.GetRiskManager()
	second, inactive := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{first, second, inactive} {
		if err := manager.AddClient(&Client{ID: id, Collateral: 100, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	futuresPnLTrade(t, engine, buyer, first, 1, 100)
	futuresPnLTrade(t, engine, buyer, second, 1, 100)
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 100); err != nil {
		t.Fatal(err)
	}
	enableTestLiquidation(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	for _, id := range []uuid.UUID{first, second} {
		status, exists := engine.GetLiquidationStatus(id, "SIM-FUT")
		if !exists || status.AttemptedQuantity != 1 || status.RemainingQuantity != 1 {
			t.Fatalf("distressed account omitted: %+v", status)
		}
	}
	account, _ := manager.GetMarginAccount(inactive)
	if account.Equity != 100 || account.RealizedPnL != 0 {
		t.Fatal("settlement changed inactive account")
	}
}
