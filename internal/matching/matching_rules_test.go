package matching

import (
	"testing"
	"time"
)

func readTestTrade(t *testing.T, engine *MatchingEngine) *Trade {
	t.Helper()
	select {
	case trade := <-engine.GetTradeChannel():
		return trade
	case <-time.After(3 * time.Second):
		t.Fatal("missing trade")
		return nil
	}
}

func TestMatchingPricePriorityAndFIFO(t *testing.T) {
	for _, side := range []Side{Bid, Ask} {
		for _, kind := range []OrderType{Limit, Market} {
			name := map[Side]string{Bid: "sell", Ask: "buy"}[side] + map[OrderType]string{Limit: "Limit", Market: "Market"}[kind]
			t.Run(name, func(t *testing.T) {
				engine := NewMatchingEngine(16, 16)
				defer engine.Close()
				worsePrice, betterPrice := uint64(90), uint64(110)
				incomingSide := Ask
				if side == Ask {
					worsePrice, betterPrice = 110, 90
					incomingSide = Bid
				}
				worse := testOrder(side, Limit, 1)
				worse.Price = worsePrice
				first := testOrder(side, Limit, 1)
				first.Price = betterPrice
				second := testOrder(side, Limit, 1)
				second.Price = betterPrice
				submitTestOrder(t, engine, worse)
				submitTestOrder(t, engine, first)
				submitTestOrder(t, engine, second)
				incoming := testOrder(incomingSide, kind, 2)
				submitTestOrder(t, engine, incoming)
				if stats := engine.GetStats(); stats.TradesProcessed != 2 {
					t.Fatalf("better prices were not matched: %+v", stats)
				}
				for _, expected := range []*Order{first, second} {
					trade := readTestTrade(t, engine)
					restingID := trade.AskOrder
					if side == Bid {
						restingID = trade.BidOrder
					}
					if restingID != expected.ID || trade.Price != betterPrice {
						t.Fatalf("price/FIFO violation: %+v", trade)
					}
				}
				remaining, _ := engine.GetOrderBook("TEST").GetOrder(worse.ID)
				if remaining == nil || remaining.Remaining != 1 {
					t.Fatal("worse price executed ahead of better price")
				}
			})
		}
	}
}

func TestOrderBookBestPricesAfterRemoval(t *testing.T) {
	book := NewOrderBook("TEST")
	bids := make([]*Order, 0, 3)
	for _, price := range []uint64{90, 110, 100} {
		order := testOrder(Bid, Limit, 1)
		order.Price = price
		book.AddOrder(order)
		bids = append(bids, order)
	}
	if best := book.GetBestBid(); best.Price != 110 {
		t.Fatalf("best bid=%d; want 110", best.Price)
	}
	book.RemoveOrder(bids[1].ID)
	if best := book.GetBestBid(); best.Price != 100 {
		t.Fatalf("best bid after cancellation=%d; want 100", best.Price)
	}
	for _, price := range []uint64{130, 120, 140} {
		order := testOrder(Ask, Limit, 1)
		order.Price = price
		book.AddOrder(order)
	}
	if best := book.GetBestAsk(); best.Price != 120 {
		t.Fatalf("best ask=%d; want 120", best.Price)
	}
}

func TestStopLimitJoinsFIFOAtActivation(t *testing.T) {
	engine := NewMatchingEngine(16, 16)
	defer engine.Close()
	stop := testOrder(Bid, StopLimit, 1)
	stop.StopPrice = 110
	submitTestOrder(t, engine, stop)
	resting := testOrder(Bid, Limit, 1)
	submitTestOrder(t, engine, resting)
	ask := testOrder(Ask, Limit, 1)
	ask.Price = 120
	submitTestOrder(t, engine, ask)
	engine.runCommand(func() { engine.checkAndTriggerStopOrders() })
	if stats := engine.GetStats(); stats.PendingStopOrders != 0 {
		t.Fatal("stop did not activate")
	}
	submitTestOrder(t, engine, testOrder(Ask, Market, 2))
	if stats := engine.GetStats(); stats.TradesProcessed != 2 {
		t.Fatalf("activated stop was not matched: %+v", stats)
	}
	for _, expected := range []*Order{resting, stop} {
		trade := readTestTrade(t, engine)
		if trade.BidOrder != expected.ID || trade.BidClient != expected.ClientID || trade.Price != 100 || trade.Quantity != 1 {
			t.Fatalf("stop activation violated FIFO: %+v", trade)
		}
	}
}
