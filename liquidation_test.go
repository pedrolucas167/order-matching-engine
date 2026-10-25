package main

import (
	"math"
	"testing"
	"time"
)

func enableTestLiquidation(t *testing.T, engine *MatchingEngine) {
	t.Helper()
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{MaxSlippageBps: 100, MaxMarkAge: time.Minute, RetryInterval: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func TestLiquidationUsesRealLiquidityAndKeepsDebt(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 100, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	enableTestLiquidation(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 350); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	status, ok := engine.GetLiquidationStatus(seller, "SIM-FUT")
	if !ok || status.FilledQuantity != 0 || status.RemainingQuantity != 1 {
		t.Fatalf("missing pending liquidation: %+v", status)
	}
	ask := derivativeTestOrder(buyer, Ask, 1)
	ask.Price = 350
	submitTestOrder(t, engine, ask)
	deadline := time.Now().Add(3 * time.Second)
	for {
		position, _ := engine.GetRiskManager().GetDerivativePosition(seller, "SIM-FUT")
		if position.NetContracts == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new liquidity did not close position")
		}
		time.Sleep(time.Millisecond)
	}
	readTestTrade(t, engine)
	status, _ = engine.GetLiquidationStatus(seller, "SIM-FUT")
	account, _ := engine.GetRiskManager().GetMarginAccount(seller)
	if status.Reason != "completed" || status.FilledQuantity != 1 || account.Equity != -150 || account.OrderMargin != 0 {
		t.Fatalf("liquidation discarded debt or reservations: %+v %+v", status, account)
	}
	before := engine.GetStats().TradesProcessed
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 350); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	if engine.GetStats().TradesProcessed != before {
		t.Fatal("repeated mark duplicated liquidation")
	}
}

func TestLiquidationProtectsPriceAndHandlesPartialFill(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 500, 1)
	futuresPnLTrade(t, engine, buyer, seller, 2, 100)
	ask := derivativeTestOrder(buyer, Ask, 1)
	ask.Price = 320
	expensive := derivativeTestOrder(buyer, Ask, 1)
	expensive.Price = 400
	submitTestOrder(t, engine, ask)
	submitTestOrder(t, engine, expensive)
	synchronizeEngine(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 320); err != nil {
		t.Fatal(err)
	}
	enableTestLiquidation(t, engine)
	synchronizeEngine(t, engine)
	status, _ := engine.GetLiquidationStatus(seller, "SIM-FUT")
	position, _ := engine.GetRiskManager().GetDerivativePosition(seller, "SIM-FUT")
	if status.FilledQuantity != 1 || position.NetContracts != -1 || status.LimitPrice != 323 {
		t.Fatalf("unexpected partial liquidation: %+v %+v", status, position)
	}
	if _, ok := engine.GetOrderBook("SIM-FUT").GetOrder(expensive.ID); !ok {
		t.Fatal("price protection consumed expensive ask")
	}
	readTestTrade(t, engine)
}

func TestLiquidationRefusesStaleMarkAndRespectsRiskControls(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 100, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	engine.runCommand(func() { engine.markTimes["SIM-FUT"] = time.Now().Add(-time.Hour) })
	enableTestLiquidation(t, engine)
	synchronizeEngine(t, engine)
	status, _ := engine.GetLiquidationStatus(seller, "SIM-FUT")
	if status.Reason != "stale price" {
		t.Fatalf("stale price used: %+v", status)
	}
	engine.GetRiskManager().SetRiskLimits(seller, &RiskLimits{MaxOrderSize: 0, MaxDailyVolume: 100})
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	engine.runCommand(func() { delete(engine.liquidations, liquidationKey{seller, "SIM-FUT"}) })
	synchronizeEngine(t, engine)
	status, _ = engine.GetLiquidationStatus(seller, "SIM-FUT")
	if status.Reason != "risk control" {
		t.Fatalf("operational limit bypassed: %+v", status)
	}
}

func TestLiquidationLimitRoundsTowardProtection(t *testing.T) {
	for _, tc := range []struct {
		mark, bps uint64
		side      Side
		want      uint64
	}{{101, 1, Ask, 101}, {101, 1, Bid, 101}, {100, 100, Ask, 99}, {100, 100, Bid, 101}, {math.MaxUint64, 0, Bid, math.MaxUint64}, {1, 10000, Ask, 1}} {
		got, err := liquidationLimit(tc.mark, tc.bps, tc.side)
		if err != nil || got != tc.want {
			t.Fatalf("limit %+v: %d %v", tc, got, err)
		}
	}
	if _, err := liquidationLimit(math.MaxUint64, 1, Bid); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestLiquidationClosesLongPosition(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 100, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	bid := derivativeTestOrder(seller, Bid, 1)
	bid.Price = 50
	submitTestOrder(t, engine, bid)
	synchronizeEngine(t, engine)
	enableTestLiquidation(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 50); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	position, _ := engine.GetRiskManager().GetDerivativePosition(buyer, "SIM-FUT")
	status, _ := engine.GetLiquidationStatus(buyer, "SIM-FUT")
	if position.NetContracts != 0 || status.Reason != "completed" || status.LimitPrice != 50 {
		t.Fatalf("long not liquidated at protected bid: %+v %+v", position, status)
	}
	readTestTrade(t, engine)
}

func TestLiquidationRequiresFreshMarksAcrossPortfolio(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 200, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "ALT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80}); err != nil {
		t.Fatal(err)
	}
	ask, bid := derivativeTestOrder(seller, Ask, 1), derivativeTestOrder(buyer, Bid, 1)
	ask.Symbol = "ALT"
	bid.Symbol = "ALT"
	ask.Price = 100
	bid.Price = 100
	submitTestOrder(t, engine, ask)
	submitTestOrder(t, engine, bid)
	synchronizeEngine(t, engine)
	readTestTrade(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 250); err != nil {
		t.Fatal(err)
	}
	enableTestLiquidation(t, engine)
	synchronizeEngine(t, engine)
	status, _ := engine.GetLiquidationStatus(seller, "SIM-FUT")
	if status.Reason != "stale price" {
		t.Fatalf("unverified portfolio mark ignored: %+v", status)
	}
}
