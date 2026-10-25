package main

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestContractAndMoneyBoundariesRejectOverflow(t *testing.T) {
	for _, tc := range []struct {
		net      int64
		quantity uint64
		side     Side
	}{
		{math.MinInt64, 1, Bid}, {0, math.MaxUint64, Ask}, {math.MaxInt64, 1, Bid}, {-math.MaxInt64, 1, Ask},
	} {
		if _, err := changeNetContracts(tc.net, tc.quantity, tc.side); !errors.Is(err, ErrRiskArithmeticOverflow) {
			t.Fatalf("unsupported net transition accepted: %+v %v", tc, err)
		}
	}
	for _, tc := range []struct{ first, second int64 }{{math.MinInt64, 0}, {0, math.MinInt64}, {math.MaxInt64, 1}, {-math.MaxInt64, -1}} {
		if _, err := addMoney(tc.first, tc.second); !errors.Is(err, ErrRiskArithmeticOverflow) {
			t.Fatalf("unsupported money sum accepted: %+v", tc)
		}
	}
	for _, position := range []DerivativePosition{{NetContracts: math.MaxInt64, OpenBuyContracts: 1}, {NetContracts: -math.MaxInt64, OpenSellContracts: 1}} {
		if _, err := calculatePositionMargin(position, 1); !errors.Is(err, ErrRiskArithmeticOverflow) {
			t.Fatalf("unreachable margin position accepted: %+v", position)
		}
	}
	account := MarginAccount{PositionMargin: math.MaxUint64}
	before := account
	if err := addMarginRequirement(&account, marginRequirement{Position: 1, Total: 1}); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatal(err)
	}
	if account != before {
		t.Fatal("overflow partially changed required margin")
	}
	if _, err := contractPnL(1, 3, 1, math.MaxInt64, true); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatal("multiplied P&L overflow accepted", err)
	}
}

func TestRejectedReservationAndDepositPreserveAccount(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, math.MaxInt64-1, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 1)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 2); err != nil {
		t.Fatal(err)
	}
	manager := engine.GetRiskManager()
	before, _ := manager.GetMarginAccount(buyer)
	if err := manager.AddClient(&Client{ID: buyer, Collateral: math.MaxInt64, MarginCurrency: "SIM"}); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatal("overflowing equity update accepted", err)
	}
	after, _ := manager.GetMarginAccount(buyer)
	if after != before {
		t.Fatal("failed deposit changed account")
	}
	order := derivativeTestOrder(buyer, Bid, 1)
	if result := manager.checkAndReserveOrder(order); !result.Approved {
		t.Fatal(result.Reason)
	}
	reserved, _ := manager.GetMarginAccount(buyer)
	if result := manager.checkAndReserveOrder(order); result.Approved {
		t.Fatal("duplicate reservation accepted")
	}
	tooLarge := derivativeTestOrder(buyer, Bid, math.MaxUint64)
	if result := manager.checkAndReserveOrder(tooLarge); result.Approved {
		t.Fatal("unsupported quantity accepted")
	}
	unchanged, _ := manager.GetMarginAccount(buyer)
	if unchanged != reserved {
		t.Fatal("failed reservation changed margin")
	}
	manager.releaseDerivativeOrder(order.ID, order.Symbol)
	legacy := NewRiskManager()
	defer legacy.Close()
	if _, exists := legacy.GetMarginAccount(uuid.New()); exists {
		t.Fatal("legacy manager exposed a derivatives margin account")
	}
}

func TestFIFOLotsCoalesceAndRejectInvalidClosures(t *testing.T) {
	position := DerivativePosition{NetContracts: 1}
	lots := []DerivativeLot{{Quantity: 1, EntryPrice: 100}}
	updated, remaining, err := applyDerivativeFill(position, lots, Bid, 2, 100, 1)
	if err != nil || updated.NetContracts != 3 || len(remaining) != 1 || remaining[0].Quantity != 3 || lots[0].Quantity != 1 {
		t.Fatalf("equal-price fills did not coalesce independently: %+v %+v %v", updated, remaining, err)
	}
	for _, tc := range []struct {
		position        DerivativePosition
		lots            []DerivativeLot
		side            Side
		quantity, price uint64
		want            error
	}{
		{DerivativePosition{NetContracts: math.MaxInt64}, nil, Bid, 1, 100, ErrRiskArithmeticOverflow},
		{DerivativePosition{NetContracts: 2}, lots, Ask, 2, 110, ErrInvalidDerivativeTrade},
		{DerivativePosition{NetContracts: 1}, lots, Ask, 1, math.MaxUint64, ErrRiskArithmeticOverflow},
		{DerivativePosition{NetContracts: 1, RealizedPnL: math.MaxInt64}, lots, Ask, 1, 101, ErrRiskArithmeticOverflow},
	} {
		if _, _, err := applyDerivativeFill(tc.position, tc.lots, tc.side, tc.quantity, tc.price, 1); !errors.Is(err, tc.want) {
			t.Fatalf("invalid closure returned %v, want %v", err, tc.want)
		}
	}
	if _, err := positionPnL(position, lots, 0, 1); !errors.Is(err, ErrUnknownMarkPrice) {
		t.Fatal("unmarked open position valued", err)
	}
}
