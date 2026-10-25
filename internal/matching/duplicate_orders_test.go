package matching

import "testing"

func TestDuplicateActiveOrderIDsPreserveOriginal(t *testing.T) {
	for _, kind := range []OrderType{Limit, Market, StopLimit} {
		name := map[OrderType]string{Limit: "limit", Market: "market", StopLimit: "stop"}[kind]
		t.Run(name, func(t *testing.T) {
			engine := NewMatchingEngine(16, 16)
			defer engine.Close()
			original := testOrder(Ask, Limit, 2)
			duplicate := testOrder(Bid, kind, 3)
			duplicate.ID = original.ID
			submitTestOrder(t, engine, original)
			submitTestOrder(t, engine, duplicate)
			stats := engine.GetStats()
			if stats.TradesProcessed != 0 || stats.PendingStopOrders != 0 {
				t.Fatalf("duplicate executed or became pending: %+v", stats)
			}
			book := engine.GetOrderBook("TEST")
			stored, exists := book.GetOrder(original.ID)
			if !exists || stored.Side != original.Side || stored.Quantity != 2 || stored.Remaining != 2 {
				t.Fatalf("duplicate replaced original: %+v", stored)
			}
			if !engine.CancelOrder(original.ID, "TEST") || engine.CancelOrder(original.ID, "TEST") {
				t.Fatal("duplicate corrupted cancellation index")
			}
			snapshot := engine.GetSnapshot("TEST")
			if snapshot.BestBid != nil || snapshot.BestAsk != nil {
				t.Fatal("unindexed duplicate remained after cancellation")
			}
		})
	}
}

func TestDuplicatePendingStopIDsAreRejected(t *testing.T) {
	for _, kind := range []OrderType{Limit, StopMarket, StopLimit} {
		name := map[OrderType]string{Limit: "resting", StopMarket: "stopMarket", StopLimit: "stopLimit"}[kind]
		t.Run(name, func(t *testing.T) {
			engine := NewMatchingEngine(16, 16)
			defer engine.Close()
			original := testOrder(Bid, StopLimit, 1)
			original.StopPrice = 200
			duplicate := testOrder(Bid, kind, 2)
			duplicate.ID = original.ID
			duplicate.StopPrice = 200
			submitTestOrder(t, engine, original)
			submitTestOrder(t, engine, duplicate)
			if stats := engine.GetStats(); stats.PendingStopOrders != 1 {
				t.Fatalf("duplicate pending stop: %+v", stats)
			}
			if order, exists := engine.GetOrderBook("TEST").GetOrder(original.ID); exists {
				t.Fatalf("duplicate became resting: %+v", order)
			}
			if !engine.CancelStopOrder(original.ID, "TEST") || engine.CancelStopOrder(original.ID, "TEST") {
				t.Fatal("duplicate stop cancelled more than once")
			}
		})
	}
}

func TestOrderBookDuplicateIDsDoNotLeaveUnindexedOrders(t *testing.T) {
	book := NewOrderBook("TEST")
	original := testOrder(Ask, Limit, 1)
	duplicate := *original
	duplicate.Price = 110
	duplicate.Remaining = 2
	duplicate.Quantity = 2
	book.AddOrder(original)
	book.AddOrder(&duplicate)
	stored, _ := book.GetOrder(original.ID)
	if stored.Price != original.Price || stored.Quantity != 1 {
		t.Fatalf("duplicate replaced original: %+v", stored)
	}
	book.RemoveOrder(original.ID)
	if book.GetBestAsk() != nil {
		t.Fatal("duplicate left an order outside the index")
	}
}
