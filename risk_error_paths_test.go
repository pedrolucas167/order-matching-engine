package main

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInvalidRiskConfigurationPreservesExistingControls(t *testing.T) {
	manager := NewRiskManager()
	defer manager.Close()
	id := uuid.New()
	if err := manager.SetRiskLimits(id, &RiskLimits{MaxOrderSize: 1}); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetRiskLimits(id, nil); !errors.Is(err, ErrInvalidRiskLimits) {
		t.Fatalf("nil limits: %v", err)
	}
	if err := manager.SetRiskLimits(uuid.Nil, &RiskLimits{}); !errors.Is(err, ErrInvalidRiskLimits) {
		t.Fatalf("missing client ID: %v", err)
	}
	if result := manager.CheckOrder(&Order{ClientID: id, Symbol: "TEST", Quantity: 2}); result.Approved {
		t.Fatal("invalid update removed existing size restriction")
	}
	original := CircuitBreaker{Symbol: "TEST", ThresholdPct: 10, LastPrice: 100, State: CircuitBreakerOpen, CooldownPeriod: int64(time.Hour)}
	if err := manager.AddCircuitBreaker(&original); err != nil {
		t.Fatal(err)
	}
	for _, breaker := range []*CircuitBreaker{nil, {}, {Symbol: " TEST", ThresholdPct: 1}, {Symbol: "TEST", ThresholdPct: 0}, {Symbol: "TEST", ThresholdPct: 1, CooldownPeriod: -1}, {Symbol: "TEST", ThresholdPct: 1, State: CircuitBreakerState(99)}} {
		if err := manager.AddCircuitBreaker(breaker); !errors.Is(err, ErrInvalidCircuitBreaker) {
			t.Errorf("invalid breaker returned %v", err)
		}
	}
	actual, _ := manager.GetCircuitBreaker("TEST")
	if *actual != original {
		t.Fatal("invalid configuration changed existing breaker")
	}
	if manager.CheckOrder(nil).Approved {
		t.Fatal("nil order approved")
	}
}

func TestCircuitBreakerUsesExactPercentageWithoutOverflow(t *testing.T) {
	cases := []struct {
		name             string
		last, price, pct uint64
		open             bool
	}{
		{"unchanged small price", 1, 1, 1, false},
		{"below fractional boundary", 101, 102, 1, false},
		{"above fractional boundary", 101, 103, 1, true},
		{"exact boundary", 100, 101, 1, true},
		{"large price below boundary", math.MaxUint64, 1, 100, false},
		{"large price exact boundary", math.MaxUint64 / 2, math.MaxUint64 - 1, 100, true},
		{"large percentage", math.MaxUint64, 1, math.MaxUint64, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewRiskManager()
			defer manager.Close()
			if err := manager.AddCircuitBreaker(&CircuitBreaker{Symbol: "TEST", ThresholdPct: tc.pct, LastPrice: tc.last, CooldownPeriod: int64(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			manager.CheckCircuitBreakerTrigger("TEST", tc.price)
			breaker, _ := manager.GetCircuitBreaker("TEST")
			if (breaker.State == CircuitBreakerOpen) != tc.open {
				t.Fatalf("unexpected breaker state: %+v", breaker)
			}
		})
	}
}

func TestLiquidationPolicyErrorsAndDisablePreservePosition(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 100, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	for _, policy := range []LiquidationPolicy{{MaxSlippageBps: 10001, MaxMarkAge: time.Second, RetryInterval: time.Second}, {MaxMarkAge: -1, RetryInterval: time.Second}, {MaxMarkAge: time.Second, RetryInterval: -1}} {
		if err := engine.SetLiquidationPolicy(policy); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	enableTestLiquidation(t, engine)
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 150); err != nil {
		t.Fatal(err)
	}
	synchronizeEngine(t, engine)
	if _, exists := engine.GetLiquidationStatus(seller, "SIM-FUT"); exists {
		t.Fatal("disabled policy attempted liquidation")
	}
	engine.Close()
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{}); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{}); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	legacy := NewMatchingEngine(1, 1)
	defer legacy.Close()
	if err := legacy.SetLiquidationPolicy(LiquidationPolicy{}); err == nil {
		t.Fatal("legacy engine accepted liquidation")
	}
}

func TestPublicSettlementErrorsLeaveAccountsAndLotsIntact(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	futuresPnLTrade(t, engine, buyer, seller, 2, 100)
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 110); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	lots, _ := engine.GetRiskManager().GetDerivativeLots(buyer, "SIM-FUT")
	for _, tc := range []struct {
		symbol string
		price  uint64
		want   error
	}{{"SIM-FUT", 0, ErrUnknownMarkPrice}, {"UNKNOWN", 110, ErrUnknownDerivativeInstrument}, {"SIM-FUT", math.MaxUint64, ErrRiskArithmeticOverflow}} {
		if err := engine.SettleDerivativeInstrument(tc.symbol, tc.price); !errors.Is(err, tc.want) {
			t.Fatalf("settlement returned %v, want %v", err, tc.want)
		}
		after, _ := engine.GetRiskManager().GetMarginAccount(buyer)
		current, _ := engine.GetRiskManager().GetDerivativeLots(buyer, "SIM-FUT")
		mark, _ := engine.GetRiskManager().GetDerivativeMarkPrice("SIM-FUT")
		if after != before || len(current) != 1 || current[0] != lots[0] || mark != 110 {
			t.Fatal("failed settlement partially changed valuation")
		}
	}
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 110); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	if after != before {
		t.Fatal("same-price settlement duplicated results")
	}
}
