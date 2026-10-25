package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

func main() {
	fmt.Println("=== Order Matching Engine Demo ===")
	fmt.Println()

	engine := NewMatchingEngine(10000, 10000)
	defer engine.Close()

	symbol := "BTC-USD"

	go func() {
		for trade := range engine.GetTradeChannel() {
			log.Printf("TRADE: %s | Price: $%.2f | Qty: %d | Bid: %s | Ask: %s\n",
				trade.Symbol,
				float64(trade.Price)/1_000_000,
				trade.Quantity,
				trade.BidOrder,
				trade.AskOrder,
			)
		}
	}()

	fmt.Println("Scenario 1: Submitting initial sell orders...")
	for i := 0; i < 5; i++ {
		order := &Order{
			ID:        uuid.New(),
			Symbol:    symbol,
			Side:      Ask,
			Type:      Limit,
			Price:     50000000 + uint64(i)*100000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		if err := engine.SubmitOrder(order); err != nil {
			log.Printf("Failed to submit ask order: %v", err)
			return
		} else {
			fmt.Printf("  Submitted Ask: $%.2f | Qty: %d\n", float64(order.Price)/1_000_000, order.Quantity)
		}
	}

	snapshot := engine.GetSnapshot(symbol)
	if snapshot != nil {
		fmt.Printf("\nOrder Book Snapshot:\n")
		if snapshot.BestBid != nil {
			fmt.Printf("  Best Bid: $%.2f (Qty: %d)\n", float64(snapshot.BestBid.Price)/1_000_000, snapshot.BestBid.TotalQty)
		} else {
			fmt.Printf("  Best Bid: None\n")
		}
		if snapshot.BestAsk != nil {
			fmt.Printf("  Best Ask: $%.2f (Qty: %d)\n", float64(snapshot.BestAsk.Price)/1_000_000, snapshot.BestAsk.TotalQty)
		} else {
			fmt.Printf("  Best Ask: None\n")
		}
	}

	// Scenario 2: Submit a bid that will match
	fmt.Println("\nScenario 2: Submitting buy order at $50.00...")
	bidOrder := &Order{
		ID:        uuid.New(),
		Symbol:    symbol,
		Side:      Bid,
		Type:      Limit,
		Price:     50000000,
		Quantity:  250,
		Remaining: 250,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine.SubmitOrder(bidOrder); err != nil {
		log.Printf("Failed to submit bid order: %v", err)
		return
	} else {
		fmt.Printf("  Submitted Bid: $%.2f | Qty: %d\n", float64(bidOrder.Price)/1_000_000, bidOrder.Quantity)
	}

	time.Sleep(100 * time.Millisecond)

	snapshot = engine.GetSnapshot(symbol)
	if snapshot != nil {
		fmt.Printf("\nOrder Book Snapshot After Matching:\n")
		if snapshot.BestBid != nil {
			fmt.Printf("  Best Bid: $%.2f (Qty: %d)\n", float64(snapshot.BestBid.Price)/1_000_000, snapshot.BestBid.TotalQty)
		} else {
			fmt.Printf("  Best Bid: None\n")
		}
		if snapshot.BestAsk != nil {
			fmt.Printf("  Best Ask: $%.2f (Qty: %d)\n", float64(snapshot.BestAsk.Price)/1_000_000, snapshot.BestAsk.TotalQty)
		} else {
			fmt.Printf("  Best Ask: None\n")
		}
	}

	stats := engine.GetStats()
	fmt.Printf("\nEngine Statistics:\n")
	fmt.Printf("  Orders Queued: %d\n", stats.OrdersProcessed)
	fmt.Printf("  Trades Processed: %d\n", stats.TradesProcessed)
	fmt.Printf("  Active Order Books: %d\n", stats.ActiveBooks)

	fmt.Println("\nScenario 3: Submitting market sell order...")
	marketOrder := &Order{
		ID:        uuid.New(),
		Symbol:    symbol,
		Side:      Ask,
		Type:      Market,
		Price:     0,
		Quantity:  50,
		Remaining: 50,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine.SubmitOrder(marketOrder); err != nil {
		log.Printf("Failed to submit market order: %v", err)
		return
	} else {
		fmt.Printf("  Submitted Market Sell: Qty: %d\n", marketOrder.Quantity)
	}

	time.Sleep(100 * time.Millisecond)

	fmt.Println("\nScenario 4: Testing order cancellation...")
	cancelOrder := &Order{
		ID:        uuid.New(),
		Symbol:    symbol,
		Side:      Bid,
		Type:      Limit,
		Price:     49000000,
		Quantity:  100,
		Remaining: 100,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine.SubmitOrder(cancelOrder); err != nil {
		log.Printf("Failed to submit cancellation candidate: %v", err)
		return
	}
	fmt.Printf("  Submitted order to cancel: %s\n", cancelOrder.ID)

	cancelled := engine.CancelOrder(cancelOrder.ID, symbol)
	if cancelled {
		fmt.Printf("  Order cancelled successfully\n")
	} else {
		fmt.Printf("  Order cancellation failed\n")
	}

	fmt.Println("\nScenario 5: Testing backpressure with rapid order submission...")
	queuedCount := 0
	backpressureRejections := 0
	for i := 0; i < 15000; i++ {
		side := Bid
		if i%2 == 0 {
			side = Ask
		}
		order := &Order{
			ID:        uuid.New(),
			Symbol:    symbol,
			Side:      side,
			Type:      Limit,
			Price:     50000000,
			Quantity:  10,
			Remaining: 10,
			Timestamp: time.Now().UnixNano(),
		}
		err := engine.SubmitOrder(order)
		switch {
		case err == nil:
			queuedCount++
		case errors.Is(err, ErrBackpressure):
			backpressureRejections++
		default:
			log.Printf("Unexpected submission failure during backpressure demo: %v", err)
			return
		}
	}
	fmt.Printf("  Orders queued: %d\n", queuedCount)
	fmt.Printf("  Backpressure rejections: %d\n", backpressureRejections)

	time.Sleep(200 * time.Millisecond)

	stats = engine.GetStats()
	fmt.Printf("\nFinal Engine Statistics:\n")
	fmt.Printf("  Orders Queued: %d\n", stats.OrdersProcessed)
	fmt.Printf("  Trades Processed: %d\n", stats.TradesProcessed)
	fmt.Printf("  Active Order Books: %d\n", stats.ActiveBooks)

	fmt.Println("\nScenario 6: Stop-Loss Order Demo...")
	fmt.Println("  Setting up market with BTC at $50.00...")

	engine2 := NewMatchingEngine(10000, 10000)
	defer engine2.Close()
	symbol2 := "ETH-USD"

	go func() {
		for trade := range engine2.GetTradeChannel() {
			log.Printf("TRADE: %s | Price: $%.2f | Qty: %d\n",
				trade.Symbol,
				float64(trade.Price)/1_000_000,
				trade.Quantity,
			)
		}
	}()

	for i := 0; i < 3; i++ {
		bid := &Order{
			ID:        uuid.New(),
			Symbol:    symbol2,
			Side:      Bid,
			Type:      Limit,
			Price:     50000000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		ask := &Order{
			ID:        uuid.New(),
			Symbol:    symbol2,
			Side:      Ask,
			Type:      Limit,
			Price:     50000000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		if err := engine2.SubmitOrder(bid); err != nil {
			log.Printf("Failed to submit stop demo bid: %v", err)
			return
		}
		if err := engine2.SubmitOrder(ask); err != nil {
			log.Printf("Failed to submit stop demo ask: %v", err)
			return
		}
	}

	time.Sleep(50 * time.Millisecond)

	fmt.Println("  Placing Stop-Loss Sell Order at $49.50...")
	stopLossOrder := &Order{
		ID:        uuid.New(),
		Symbol:    symbol2,
		Side:      Ask,
		Type:      StopMarket,
		Price:     0,
		StopPrice: 49500000,
		Quantity:  50,
		Remaining: 50,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine2.SubmitOrder(stopLossOrder); err != nil {
		log.Printf("Failed to submit stop-loss order: %v", err)
		return
	}
	fmt.Printf("  Stop-Loss Order ID: %s\n", stopLossOrder.ID)

	time.Sleep(50 * time.Millisecond)

	stats2 := engine2.GetStats()
	fmt.Printf("  Pending Stop Orders: %d\n", stats2.PendingStopOrders)

	fmt.Println("  Dropping price to $49.00 by aggressive selling...")
	for i := 0; i < 5; i++ {
		ask := &Order{
			ID:        uuid.New(),
			Symbol:    symbol2,
			Side:      Ask,
			Type:      Limit,
			Price:     49000000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		if err := engine2.SubmitOrder(ask); err != nil {
			log.Printf("Failed to submit stop demo ask: %v", err)
			return
		}
	}

	time.Sleep(150 * time.Millisecond)

	stats2 = engine2.GetStats()
	fmt.Printf("  Pending Stop Orders after price drop: %d\n", stats2.PendingStopOrders)
	fmt.Printf("  Stop-Loss should have been triggered!\n")

	snapshot2 := engine2.GetSnapshot(symbol2)
	if snapshot2 != nil && snapshot2.BestBid != nil {
		fmt.Printf("  Current Best Bid: $%.2f\n", float64(snapshot2.BestBid.Price)/1_000_000)
	}

	fmt.Println("\nScenario 7: Risk Management Demo...")
	engine3 := NewMatchingEngine(10000, 10000)
	defer engine3.Close()
	symbol3 := "PETR4-USD"

	go func() {
		for trade := range engine3.GetTradeChannel() {
			log.Printf("TRADE: %s | Price: $%.2f | Qty: %d\n",
				trade.Symbol,
				float64(trade.Price)/1_000_000,
				trade.Quantity,
			)
		}
	}()

	client1ID := uuid.New()
	client2ID := uuid.New()

	riskManager := engine3.GetRiskManager()

	client1 := &Client{
		ID:            client1ID,
		Name:          "Client A - High Risk",
		Collateral:    10000000000,
		Exposure:      0,
		LongPosition:  0,
		ShortPosition: 0,
	}

	client2 := &Client{
		ID:            client2ID,
		Name:          "Client B - Conservative",
		Collateral:    10000000000,
		Exposure:      0,
		LongPosition:  0,
		ShortPosition: 0,
	}

	riskManager.AddClient(client1)
	riskManager.AddClient(client2)

	limits := &RiskLimits{
		MaxOrderSize:    1000,
		MaxDailyVolume:  10000,
		MaxExposure:     50,
		MaxPositionSize: 5000,
	}
	riskManager.SetRiskLimits(client2ID, limits)

	breaker := &CircuitBreaker{
		Symbol:         symbol3,
		State:          CircuitBreakerClosed,
		ThresholdPct:   20,
		LastPrice:      30000000,
		CooldownPeriod: 30 * 1000000000,
	}
	riskManager.AddCircuitBreaker(breaker)

	fmt.Printf("  Created %s with $10,000 collateral\n", client1.Name)
	fmt.Printf("  Created %s with $10,000 collateral and strict limits\n", client2.Name)
	fmt.Printf("  Added circuit breaker for %s (20%% threshold)\n", symbol3)

	fmt.Println("\n  Test 1: Client A places large order (no limits)...")
	order1 := &Order{
		ID:        uuid.New(),
		ClientID:  client1ID,
		Symbol:    symbol3,
		Side:      Bid,
		Type:      Limit,
		Price:     30000000,
		Quantity:  5000,
		Remaining: 5000,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine3.SubmitOrder(order1); err != nil {
		log.Printf("Failed to queue risk demo order: %v", err)
		return
	}
	fmt.Println("    Order queued for asynchronous risk checks")

	fmt.Println("\n  Test 2: Client B tries order exceeding size limit...")
	order2 := &Order{
		ID:        uuid.New(),
		ClientID:  client2ID,
		Symbol:    symbol3,
		Side:      Bid,
		Type:      Limit,
		Price:     30000000,
		Quantity:  2000,
		Remaining: 2000,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine3.SubmitOrder(order2); err != nil {
		log.Printf("Failed to queue risk demo order: %v", err)
		return
	}
	fmt.Println("    Order queued for asynchronous risk checks")

	fmt.Println("\n  Test 3: Client B places order within limits...")
	order3 := &Order{
		ID:        uuid.New(),
		ClientID:  client2ID,
		Symbol:    symbol3,
		Side:      Bid,
		Type:      Limit,
		Price:     30000000,
		Quantity:  500,
		Remaining: 500,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine3.SubmitOrder(order3); err != nil {
		log.Printf("Failed to queue risk demo order: %v", err)
		return
	}
	fmt.Println("    Order queued for asynchronous risk checks")

	time.Sleep(50 * time.Millisecond)

	fmt.Println("\n  Test 4: Check client positions...")
	if c1, ok := riskManager.GetClient(client1ID); ok {
		fmt.Printf("    %s: Long Position: %d, Exposure: $%.2f\n",
			c1.Name,
			c1.LongPosition,
			float64(c1.Exposure)/1_000_000,
		)
	}
	if c2, ok := riskManager.GetClient(client2ID); ok {
		fmt.Printf("    %s: Long Position: %d, Exposure: $%.2f\n",
			c2.Name,
			c2.LongPosition,
			float64(c2.Exposure)/1_000_000,
		)
	}

	fmt.Println("\n  Test 5: Trigger circuit breaker with 25% price move...")
	for i := 0; i < 10; i++ {
		ask := &Order{
			ID:        uuid.New(),
			ClientID:  client1ID,
			Symbol:    symbol3,
			Side:      Ask,
			Type:      Limit,
			Price:     37500000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		bid := &Order{
			ID:        uuid.New(),
			ClientID:  client1ID,
			Symbol:    symbol3,
			Side:      Bid,
			Type:      Limit,
			Price:     37500000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		if err := engine3.SubmitOrder(ask); err != nil {
			log.Printf("Failed to submit circuit breaker demo ask: %v", err)
			return
		}
		if err := engine3.SubmitOrder(bid); err != nil {
			log.Printf("Failed to submit circuit breaker demo bid: %v", err)
			return
		}
	}

	time.Sleep(100 * time.Millisecond)

	if cb, ok := riskManager.GetCircuitBreaker(symbol3); ok {
		fmt.Printf("    Circuit Breaker State: ")
		switch cb.State {
		case CircuitBreakerClosed:
			fmt.Printf("Closed (trading allowed)\n")
		case CircuitBreakerOpen:
			fmt.Printf("OPEN (trading halted)\n")
		case CircuitBreakerHalfOpen:
			fmt.Printf("Half-Open (limited trading)\n")
		}
	}

	fmt.Println("\n  Test 6: Try to trade when circuit breaker is open...")
	order4 := &Order{
		ID:        uuid.New(),
		ClientID:  client1ID,
		Symbol:    symbol3,
		Side:      Bid,
		Type:      Limit,
		Price:     38000000,
		Quantity:  100,
		Remaining: 100,
		Timestamp: time.Now().UnixNano(),
	}
	if err := engine3.SubmitOrder(order4); err != nil {
		log.Printf("Failed to queue risk demo order: %v", err)
		return
	}
	fmt.Println("    Order queued for asynchronous risk checks")

	queuedOrders := engine3.GetStats().OrdersProcessed
	riskStats := riskManager.GetStats()
	fmt.Printf("\n  Risk Management Statistics:\n")
	fmt.Printf("    Orders Queued: %d\n", queuedOrders)
	fmt.Printf("    Orders Rejected: %d\n", riskStats.OrdersRejected)
	fmt.Printf("    Orders Blocked by Circuit Breaker: %d\n", riskStats.OrdersBlockedByCB)
	fmt.Printf("    Exposure Breaches: %d\n", riskStats.ExposureBreaches)
	fmt.Printf("    Collateral Breaches: %d\n", riskStats.CollateralBreaches)

	snapshot = engine.GetSnapshot(symbol)
	if snapshot != nil {
		fmt.Printf("\nFinal Order Book Snapshot:\n")
		if snapshot.BestBid != nil {
			fmt.Printf("  Best Bid: $%.2f (Qty: %d)\n", float64(snapshot.BestBid.Price)/1_000_000, snapshot.BestBid.TotalQty)
		}
		if snapshot.BestAsk != nil {
			fmt.Printf("  Best Ask: $%.2f (Qty: %d)\n", float64(snapshot.BestAsk.Price)/1_000_000, snapshot.BestAsk.TotalQty)
		}
	}

	fmt.Println("\n=== Demo Complete ===")
}
