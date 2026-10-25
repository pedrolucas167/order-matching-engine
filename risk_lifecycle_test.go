package main

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDailyVolumeResetRestoresOnlyTurnoverBudget(t *testing.T) {
	manager := NewRiskManager()
	defer manager.Close()
	client, other := uuid.New(), uuid.New()
	if err := manager.AddClient(&Client{ID: client, Collateral: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetRiskLimits(client, &RiskLimits{MaxOrderSize: 10, MaxDailyVolume: 2, MaxExposure: 100}); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpdatePosition(&Trade{BidClient: client, AskClient: other, Symbol: "TEST", Quantity: 2, Price: 100}); err != nil {
		t.Fatal(err)
	}
	order := &Order{ClientID: client, Symbol: "TEST", Side: Bid, Quantity: 1, Price: 100}
	if manager.CheckOrder(order).Approved {
		t.Fatal("filled turnover did not consume daily budget")
	}
	before, _ := manager.GetClient(client)
	manager.ResetDailyVolume(client)
	if !manager.CheckOrder(order).Approved {
		t.Fatal("reset did not restore turnover budget")
	}
	after, _ := manager.GetClient(client)
	if *after != *before {
		t.Fatal("volume reset changed funding or positions")
	}
	if err := manager.UpdatePosition(nil); !errors.Is(err, ErrInvalidDerivativeTrade) {
		t.Fatal(err)
	}
	if err := manager.AddClient(nil); !errors.Is(err, ErrClientNotFound) {
		t.Fatal(err)
	}
	if err := manager.AddClient(&Client{}); !errors.Is(err, ErrClientNotFound) {
		t.Fatal(err)
	}
	if _, exists := manager.GetClient(other); exists {
		t.Fatal("unknown client appeared")
	}
	if _, exists := manager.GetCircuitBreaker("UNKNOWN"); exists {
		t.Fatal("unknown breaker appeared")
	}
}

func TestCircuitBreakerCooldownResumesTradingWithoutChangingTrigger(t *testing.T) {
	manager := NewRiskManager()
	defer manager.Close()
	if err := manager.AddCircuitBreaker(&CircuitBreaker{Symbol: "TEST", ThresholdPct: 10, LastPrice: 100, CooldownPeriod: int64(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	manager.CheckCircuitBreakerTrigger("TEST", 110)
	triggered, _ := manager.GetCircuitBreaker("TEST")
	manager.CheckCircuitBreakerTrigger("TEST", 120)
	unchanged, _ := manager.GetCircuitBreaker("TEST")
	if *unchanged != *triggered {
		t.Fatal("open breaker accepted a new trigger")
	}
	manager.checkCooldowns()
	if manager.CheckOrder(&Order{Symbol: "TEST"}).Approved {
		t.Fatal("breaker recovered before cooldown")
	}
	expired := *triggered
	expired.OpenTime = time.Now().Add(-2 * time.Hour).UnixNano()
	if err := manager.AddCircuitBreaker(&expired); err != nil {
		t.Fatal(err)
	}
	manager.checkCooldowns()
	if !manager.CheckOrder(&Order{Symbol: "TEST"}).Approved {
		t.Fatal("expired cooldown did not restore trading")
	}
	current, _ := manager.GetCircuitBreaker("TEST")
	if current.OpenTime != 0 || current.TriggerPrice != triggered.TriggerPrice {
		t.Fatal("cooldown changed historical trigger")
	}
}

func TestMarginDeficitCancelsOnlyAffectedStopReservations(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 500, 1)
	futuresPnLTrade(t, engine, buyer, seller, 2, 100)
	stops := []*Order{derivativeTestOrder(buyer, Bid, 1), derivativeTestOrder(seller, Bid, 1)}
	for _, order := range stops {
		order.Type = StopLimit
		order.StopPrice = 2000
		submitTestOrder(t, engine, order)
	}
	if count := engine.GetStats().PendingStopOrders; count != 2 {
		t.Fatalf("stops not admitted: %d", count)
	}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 320); err != nil {
		t.Fatal(err)
	}
	if count := engine.GetStats().PendingStopOrders; count != 1 {
		t.Fatalf("wrong stop count after deficit: %d", count)
	}
	if engine.CancelStopOrder(stops[1].ID, "SIM-FUT") {
		t.Fatal("deficit stop remained live")
	}
	if !engine.CancelStopOrder(stops[0].ID, "SIM-FUT") {
		t.Fatal("healthy account stop was canceled")
	}
	position, _ := engine.GetRiskManager().GetDerivativePosition(seller, "SIM-FUT")
	if position.NetContracts != -2 || position.OpenBuyContracts != 0 {
		t.Fatalf("cancellation changed position or leaked reservation: %+v", position)
	}
}
