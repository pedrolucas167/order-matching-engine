package main

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestRiskManagerConcurrentAccessAndCopies(t *testing.T) {
	manager := NewRiskManager()
	defer manager.Close()
	client := &Client{ID: uuid.New(), Collateral: 1000000}
	manager.AddClient(client)
	client.Collateral = 0
	limits := &RiskLimits{MaxOrderSize: 100, MaxDailyVolume: 1000000, MaxExposure: 100}
	manager.SetRiskLimits(client.ID, limits)
	limits.MaxOrderSize = 0
	breaker := &CircuitBreaker{Symbol: "TEST", ThresholdPct: 50}
	manager.AddCircuitBreaker(breaker)
	breaker.State = CircuitBreakerOpen
	initialOrder := &Order{ClientID: client.ID, Side: Bid, Symbol: "TEST", Quantity: 1, Price: 100}
	if result := manager.CheckOrder(initialOrder); !result.Approved {
		t.Fatalf("input mutation affected risk checks: %+v", result)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := &Order{Side: Bid, Type: Limit, Quantity: 1, Price: 100, Symbol: "TEST"}
			order.ClientID = client.ID
			manager.CheckOrder(order)
			manager.UpdatePosition(&Trade{BidClient: client.ID, Symbol: "TEST", Quantity: 1, Price: 100})
			manager.CheckCircuitBreakerTrigger("TEST", 100)
			manager.checkCooldowns()
			manager.AddClient(&Client{ID: uuid.New()})
			read, _ := manager.GetClient(client.ID)
			read.Collateral = 0
			cb, _ := manager.GetCircuitBreaker("TEST")
			cb.State = CircuitBreakerOpen
		}()
	}
	wg.Wait()
	read, _ := manager.GetClient(client.ID)
	if read.LongPosition != 20 || read.Collateral != 1000000 {
		t.Fatalf("client state %+v", read)
	}
}
