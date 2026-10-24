package main

import "testing"

func TestStopReferencePriceDoesNotOverflow(t *testing.T) {
	for _, side := range []Side{Bid, Ask} {
		name := map[Side]string{Bid: "buyTriggers", Ask: "sellRemainsPending"}[side]
		t.Run(name, func(t *testing.T) {
			engine := NewMatchingEngine(16, 16)
			defer engine.Close()
			maximumPrice := ^uint64(0)
			bid := testOrder(Bid, Limit, 1)
			bid.Price = maximumPrice - 20
			ask := testOrder(Ask, Limit, 1)
			ask.Price = maximumPrice - 10
			submitTestOrder(t, engine, bid)
			submitTestOrder(t, engine, ask)
			stop := testOrder(side, StopMarket, 1)
			stop.StopPrice = maximumPrice - 15
			if side == Ask {
				stop.StopPrice--
			}
			submitTestOrder(t, engine, stop)
			engine.runCommand(func() { engine.checkAndTriggerStopOrders() })
			stats := engine.GetStats()
			if side == Bid {
				if stats.TradesProcessed != 1 || stats.PendingStopOrders != 0 {
					t.Fatalf("buy stop did not trigger at midpoint: %+v", stats)
				}
				trade := readTestTrade(t, engine)
				if trade.Price != ask.Price {
					t.Fatalf("unexpected stop execution price %d", trade.Price)
				}
			} else if stats.TradesProcessed != 0 || stats.PendingStopOrders != 1 {
				t.Fatalf("sell stop triggered below the midpoint: %+v", stats)
			}
		})
	}
}
