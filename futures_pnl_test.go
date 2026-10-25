package main

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func newFuturesPnLTest(t *testing.T, collateral uint64, multiplier uint64) (*MatchingEngine, uuid.UUID, uuid.UUID) {
	t.Helper()
	engine := NewDerivativesMatchingEngine(512, 512)
	t.Cleanup(engine.Close)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80, ContractMultiplier: multiplier}); err != nil {
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

func futuresPnLTrade(t *testing.T, engine *MatchingEngine, buyer, seller uuid.UUID, quantity, price uint64) {
	t.Helper()
	ask, bid := derivativeTestOrder(seller, Ask, quantity), derivativeTestOrder(buyer, Bid, quantity)
	ask.Price = price
	bid.Price = price
	before := engine.GetStats().TradesProcessed
	submitTestOrder(t, engine, ask)
	submitTestOrder(t, engine, bid)
	if after := engine.GetStats().TradesProcessed; after != before+1 {
		t.Fatalf("trade did not execute: before=%d after=%d", before, after)
	}
	readTestTrade(t, engine)
}

func TestFuturesPnLMarkCloseAndDailySettlement(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 10)
	manager := engine.GetRiskManager()
	futuresPnLTrade(t, engine, buyer, seller, 2, 100)
	if err := manager.setDerivativeMarkPrice("SIM-FUT", 110); err != nil {
		t.Fatal(err)
	}
	long, _ := manager.GetMarginAccount(buyer)
	short, _ := manager.GetMarginAccount(seller)
	if long.UnrealizedPnL != 200 || short.UnrealizedPnL != -200 || long.Equity != 1200 || short.Equity != 800 || long.MaintenanceMargin != 160 || short.MaintenanceMargin != 160 {
		t.Fatalf("marked accounts: %+v %+v", long, short)
	}
	futuresPnLTrade(t, engine, seller, buyer, 1, 120)
	long, _ = manager.GetMarginAccount(buyer)
	short, _ = manager.GetMarginAccount(seller)
	if long.RealizedPnL != 200 || long.UnrealizedPnL != 100 || long.Equity != 1300 || short.Equity != 700 {
		t.Fatalf("partial closing accounts: %+v %+v", long, short)
	}
	if err := manager.settleDerivativeInstrument("SIM-FUT", 115); err != nil {
		t.Fatal(err)
	}
	long, _ = manager.GetMarginAccount(buyer)
	short, _ = manager.GetMarginAccount(seller)
	if long.RealizedPnL != 350 || short.RealizedPnL != -350 || long.UnrealizedPnL != 0 || short.UnrealizedPnL != 0 || long.Equity+short.Equity != 2000 {
		t.Fatalf("variation settlement: %+v %+v", long, short)
	}
	beforeLong, beforeShort := long, short
	if err := manager.settleDerivativeInstrument("SIM-FUT", 115); err != nil {
		t.Fatal(err)
	}
	long, _ = manager.GetMarginAccount(buyer)
	short, _ = manager.GetMarginAccount(seller)
	if long != beforeLong || short != beforeShort {
		t.Fatal("repeated settlement counted P&L twice")
	}
	lots, _ := manager.GetDerivativeLots(buyer, "SIM-FUT")
	if len(lots) != 1 || lots[0].EntryPrice != 115 || lots[0].Quantity != 1 {
		t.Fatalf("basis was not reset: %+v", lots)
	}
	lots[0].EntryPrice = 999
	fresh, _ := manager.GetDerivativeLots(buyer, "SIM-FUT")
	if fresh[0].EntryPrice != 115 {
		t.Fatal("cost basis snapshot exposed internal lots")
	}
}

func TestFuturesPnLFIFOBasisAndPortfolioConservation(t *testing.T) {
	engine, trader, firstSeller := newFuturesPnLTest(t, 1000, 1)
	manager := engine.GetRiskManager()
	secondSeller, newBuyer := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{secondSeller, newBuyer} {
		if err := manager.AddClient(&Client{ID: id, Collateral: 1000, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	futuresPnLTrade(t, engine, trader, firstSeller, 1, 100)
	futuresPnLTrade(t, engine, trader, secondSeller, 1, 110)
	futuresPnLTrade(t, engine, newBuyer, trader, 1, 120)
	position, _ := manager.GetDerivativePosition(trader, "SIM-FUT")
	lots, _ := manager.GetDerivativeLots(trader, "SIM-FUT")
	if position.RealizedPnL != 20 || position.NetContracts != 1 || len(lots) != 1 || lots[0].EntryPrice != 110 {
		t.Fatalf("FIFO basis mismatch: %+v %+v", position, lots)
	}
	if err := manager.setDerivativeMarkPrice("SIM-FUT", 125); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, id := range []uuid.UUID{trader, firstSeller, secondSeller, newBuyer} {
		account, ok := manager.GetMarginAccount(id)
		if !ok {
			t.Fatal("account disappeared")
		}
		total += account.Equity
	}
	if total != 4000 {
		t.Fatalf("trade P&L was not zero-sum: total equity=%d", total)
	}
}

func TestFuturesMarkAndSettlementOverflowRollBack(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	manager := engine.GetRiskManager()
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	if err := manager.AddClient(&Client{ID: seller, Collateral: math.MaxInt64, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	before, _ := manager.GetMarginAccount(buyer)
	beforePosition, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	if err := manager.setDerivativeMarkPrice("SIM-FUT", 1); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatalf("overflowing mark returned %v", err)
	}
	if err := manager.settleDerivativeInstrument("SIM-FUT", 1); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatalf("overflowing settlement returned %v", err)
	}
	mark, _ := manager.GetDerivativeMarkPrice("SIM-FUT")
	after, _ := manager.GetMarginAccount(buyer)
	afterPosition, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	if mark != 100 || before != after || beforePosition != afterPosition {
		t.Fatal("failed valuation partially changed accounts or mark")
	}
}

func TestFuturesPortfolioPnLSumIsOrderIndependent(t *testing.T) {
	manager, client, _ := newDerivativeRiskTest(t, 1, 0)
	for _, symbol := range []string{"A", "B", "C"} {
		if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: symbol, MarginCurrency: "SIM", InitialMarginPerContract: 1}); err != nil {
			t.Fatal(err)
		}
	}
	manager.accountMu.Lock()
	manager.derivativePositions[client] = map[string]DerivativePosition{"A": {RealizedPnL: math.MaxInt64}, "B": {RealizedPnL: 1}, "C": {RealizedPnL: -1}}
	manager.accountMu.Unlock()
	for i := 0; i < 50; i++ {
		account, ok := manager.GetMarginAccount(client)
		if !ok || account.RealizedPnL != math.MaxInt64 || account.Equity != math.MaxInt64 {
			t.Fatalf("map iteration changed the financial result: %+v", account)
		}
	}
}
