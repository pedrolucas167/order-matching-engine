package main

import "testing"

func TestFillStatusesAndRemainingQuantities(t *testing.T) {
	for _, side := range []Side{Bid, Ask} {
		for _, kind := range []OrderType{Limit, Market} {
			for _, quantity := range []uint64{2, 5, 8} {
				name := map[Side]string{Bid: "sell", Ask: "buy"}[side] + map[OrderType]string{Limit: "Limit", Market: "Market"}[kind] + map[uint64]string{2: "PartialMaker", 5: "Full", 8: "PartialTaker"}[quantity]
				t.Run(name, func(t *testing.T) {
					engine := NewMatchingEngine(16, 16)
					defer engine.Close()
					resting := testOrder(side, Limit, 5)
					submitTestOrder(t, engine, resting)
					incomingSide := Ask
					if side == Ask {
						incomingSide = Bid
					}
					incoming := testOrder(incomingSide, kind, quantity)
					var maker Order
					var levelQuantity uint64
					var takerLevelQuantity uint64
					engine.runCommand(func() {
						book := engine.books["TEST"]
						restingOrder := book.orders[resting.ID]
						engine.processOrder(incoming)
						maker = *restingOrder
						if incoming.Type == Limit && incoming.Remaining > 0 {
							takerLevel := book.getBestBid()
							if incoming.Side == Ask {
								takerLevel = book.getBestAsk()
							}
							takerLevelQuantity = takerLevel.TotalQty
						}
						level := book.getBestAsk()
						if side == Bid {
							level = book.getBestBid()
						}
						if level != nil {
							levelQuantity = level.TotalQty
						}
					})
					filled := min(quantity, 5)
					if maker.Remaining != 5-filled {
						t.Fatalf("maker remaining=%d", maker.Remaining)
					}
					makerStatus := Filled
					if quantity < 5 {
						makerStatus = PartialFilled
					}
					if maker.Status != makerStatus {
						t.Fatalf("maker status=%v; want %v", maker.Status, makerStatus)
					}
					takerStatus := Filled
					if quantity > 5 {
						takerStatus = PartialFilled
						if kind == Market {
							takerStatus = Rejected
						}
					}
					if incoming.Remaining != quantity-filled || incoming.Status != takerStatus {
						t.Fatalf("taker remaining=%d status=%v; want remaining=%d status=%v", incoming.Remaining, incoming.Status, quantity-filled, takerStatus)
					}
					if levelQuantity != 5-filled {
						t.Fatalf("maker level total=%d; want %d", levelQuantity, 5-filled)
					}
					trade := readTestTrade(t, engine)
					if trade.Quantity != filled {
						t.Fatalf("trade quantity=%d; want %d", trade.Quantity, filled)
					}
					if quantity > 5 && kind == Limit {
						if takerLevelQuantity != 3 {
							t.Fatalf("resting taker level total=%d; want 3", takerLevelQuantity)
						}
						result, _ := engine.GetOrderBook("TEST").GetOrder(incoming.ID)
						if result == nil || result.Remaining != 3 || result.Status != PartialFilled {
							t.Fatalf("missing partial taker: %+v", result)
						}
						if !engine.CancelOrder(incoming.ID, "TEST") {
							t.Fatal("partial taker could not be cancelled")
						}
					}
					if quantity < 5 && !engine.CancelOrder(resting.ID, "TEST") {
						t.Fatal("partial maker could not be cancelled")
					}
					snapshot := engine.GetSnapshot("TEST")
					if snapshot.BestBid != nil || snapshot.BestAsk != nil {
						t.Fatal("filled or cancelled quantity remained")
					}
				})
			}
		}
	}
}

func TestTriggeredStopStateMatchesExecution(t *testing.T) {
	for _, kind := range []OrderType{StopMarket, StopLimit} {
		for _, quantity := range []uint64{1, 3} {
			name := map[OrderType]string{StopMarket: "market", StopLimit: "limit"}[kind] + map[uint64]string{1: "Full", 3: "Partial"}[quantity]
			t.Run(name, func(t *testing.T) {
				engine := NewMatchingEngine(16, 16)
				defer engine.Close()
				submitTestOrder(t, engine, testOrder(Ask, Limit, 2))
				stop := testOrder(Bid, kind, quantity)
				engine.runCommand(func() { engine.triggerStopOrder(stop) })
				expectedRemaining := uint64(0)
				expectedStatus := Filled
				if quantity == 3 {
					expectedRemaining = 1
					expectedStatus = PartialFilled
					if kind == StopMarket {
						expectedStatus = Rejected
					}
				}
				if stop.Remaining != expectedRemaining || stop.Status != expectedStatus {
					t.Fatalf("stop remaining=%d status=%v; want remaining=%d status=%v", stop.Remaining, stop.Status, expectedRemaining, expectedStatus)
				}
				trade := readTestTrade(t, engine)
				if trade.BidClient != stop.ClientID || trade.Quantity != min(quantity, 2) {
					t.Fatalf("invalid stop trade: %+v", trade)
				}
			})
		}
	}
}
