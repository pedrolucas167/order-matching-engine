package matching

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func waitDurableTrades(t *testing.T, engine *DurableMatchingEngine, count uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		state, err := engine.GetTradeDeliveryState()
		if err != nil {
			t.Fatal(err)
		}
		if state.Latest == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable maintenance did not execute: %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDurableLiquidationRequiresFreshReferenceAfterRecovery(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	if err := engine.AddClient(&Client{ID: seller, Collateral: 100, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	ask, bid := derivativeTestOrder(seller, Ask, 1), derivativeTestOrder(buyer, Bid, 1)
	ask.Price = 100
	bid.Price = 100
	durableSubmit(t, engine, "maker", ask)
	durableSubmit(t, engine, "taker", bid)
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{MaxSlippageBps: 100, MaxMarkAge: time.Minute, RetryInterval: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	closing := derivativeTestOrder(buyer, Ask, 1)
	closing.Price = 150
	durableSubmit(t, recovered, "liquidity", closing)
	time.Sleep(120 * time.Millisecond)
	state, err := recovered.GetTradeDeliveryState()
	if err != nil || state.Latest != 1 {
		t.Fatal("recovered price triggered forced liquidation without refresh", err)
	}
	if err := recovered.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	waitDurableTrades(t, recovered, 2)
	recovered.Close()
	last := reopenDurableTest(t, path)
	position, exists, err := last.GetDerivativePosition(seller, "SIM-FUT")
	account, accountError := last.GetMarginAccount(seller)
	if err != nil || accountError != nil || !exists || position.NetContracts != 0 || account.Equity != 50 || account.RealizedPnL != -50 {
		t.Fatalf("liquidation did not recover coherently: %+v %+v %v %v", position, account, err, accountError)
	}
}

func TestDurableStopTriggerAndCooldownCommitOnMaintenance(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Price = 100
	durableSubmit(t, engine, "bid", bid)
	stop := derivativeTestOrder(seller, Ask, 1)
	stop.Type = StopMarket
	stop.StopPrice = 90
	stop.Price = 0
	original := durableSubmit(t, engine, "stop", stop)
	engine.Close()
	recovered := reopenDurableTest(t, path)
	if _, err := recovered.CancelOrder(bid.ID, bid.Symbol); err != nil {
		t.Fatal(err)
	}
	next := derivativeTestOrder(buyer, Bid, 1)
	next.Price = 80
	durableSubmit(t, recovered, "lower-bid", next)
	waitDurableTrades(t, recovered, 1)
	if err := recovered.AddCircuitBreaker(&CircuitBreaker{Symbol: "OTHER", State: CircuitBreakerOpen, ThresholdPct: 10, OpenTime: time.Now().Add(-time.Hour).UnixNano(), CooldownPeriod: int64(time.Second)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		recovered.mu.Lock()
		breaker, _ := recovered.engine.riskManager.GetCircuitBreaker("OTHER")
		recovered.mu.Unlock()
		if breaker.State == CircuitBreakerClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed cooldown did not commit")
		}
		time.Sleep(time.Millisecond)
	}
	recovered.Close()
	last := reopenDurableTest(t, path)
	events, err := last.ReadTrades(0, 1)
	if err != nil || len(events) != 1 || events[0].Trade.Price != 80 || events[0].Trade.AskOrder != stop.ID {
		t.Fatal("stop execution was not durable", err)
	}
	if receipt, replayed, err := last.SubmitOrderRequest(context.Background(), "stop", stop); err != nil || !replayed || receipt != original {
		t.Fatal("executed stop lost original receipt", err)
	}
	if err := last.SetRiskLimits(uuid.New(), &RiskLimits{MaxOrderSize: 1}); err != nil {
		t.Fatal(err)
	}
}
