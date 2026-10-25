package main

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestSubmitOrderRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Order)
		field  string
	}{
		{"missingID", func(order *Order) { order.ID = uuid.Nil }, "ID"},
		{"emptySymbol", func(order *Order) { order.Symbol = "" }, "symbol"},
		{"blankSymbol", func(order *Order) { order.Symbol = " \t " }, "symbol"},
		{"untrimmedSymbol", func(order *Order) { order.Symbol = " TEST " }, "symbol"},
		{"negativeSide", func(order *Order) { order.Side = Side(-1) }, "side"},
		{"unknownSide", func(order *Order) { order.Side = Side(2) }, "side"},
		{"negativeType", func(order *Order) { order.Type = OrderType(-1) }, "type"},
		{"unknownType", func(order *Order) { order.Type = OrderType(4) }, "type"},
		{"zeroQuantity", func(order *Order) { order.Quantity = 0; order.Remaining = 0 }, "quantity"},
		{"zeroRemaining", func(order *Order) { order.Remaining = 0 }, "remaining"},
		{"previouslyFilledQuantity", func(order *Order) { order.Remaining = 1 }, "remaining"},
		{"excessRemaining", func(order *Order) { order.Remaining = 3 }, "remaining"},
		{"zeroLimitPrice", func(order *Order) { order.Price = 0 }, "price"},
		{"zeroStopLimitPrice", func(order *Order) { order.Type = StopLimit; order.Price = 0 }, "price"},
		{"zeroStopMarketTrigger", func(order *Order) { order.Type = StopMarket; order.StopPrice = 0 }, "stop price"},
		{"zeroStopLimitTrigger", func(order *Order) { order.Type = StopLimit; order.StopPrice = 0 }, "stop price"},
		{"partiallyFilledStatus", func(order *Order) { order.Status = PartialFilled }, "status"},
		{"filledStatus", func(order *Order) { order.Status = Filled }, "status"},
		{"cancelledStatus", func(order *Order) { order.Status = Cancelled }, "status"},
		{"rejectedStatus", func(order *Order) { order.Status = Rejected }, "status"},
		{"triggeredStatus", func(order *Order) { order.Status = Triggered }, "status"},
		{"negativeStatus", func(order *Order) { order.Status = OrderStatus(-1) }, "status"},
		{"unknownStatus", func(order *Order) { order.Status = OrderStatus(7) }, "status"},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			engine := NewMatchingEngine(2, 2)
			defer engine.Close()
			order := testOrder(Bid, Limit, 2)
			scenario.change(order)
			original := *order
			err := engine.SubmitOrder(order)
			if !errors.Is(err, ErrInvalidOrder) {
				t.Fatalf("expected ErrInvalidOrder; got %v", err)
			}
			if !strings.Contains(err.Error(), scenario.field) {
				t.Fatalf("error %q does not identify %q", err, scenario.field)
			}
			if *order != original {
				t.Fatal("validation changed caller input")
			}
			if stats := engine.GetStats(); stats != (EngineStats{}) {
				t.Fatalf("invalid input changed engine state: %+v", stats)
			}
			if stats := engine.GetRiskManager().GetStats(); stats != (RiskStats{}) {
				t.Fatalf("invalid input reached risk checks: %+v", stats)
			}
		})
	}
}

func TestMalformedQuantityCannotConsumeLiquidity(t *testing.T) {
	engine := NewMatchingEngine(8, 8)
	defer engine.Close()
	resting := testOrder(Ask, Limit, 5)
	submitTestOrder(t, engine, resting)
	malformed := testOrder(Bid, Market, 1)
	malformed.Remaining = 5
	if err := engine.SubmitOrder(malformed); err == nil {
		t.Fatal("quantity smaller than remaining was accepted")
	}
	if stats := engine.GetStats(); stats.TradesProcessed != 0 || stats.OrdersProcessed != 1 {
		t.Fatalf("invalid order affected matching: %+v", stats)
	}
	snapshot := engine.GetSnapshot("TEST")
	if snapshot.BestAsk.TotalQty != 5 {
		t.Fatal("invalid quantity consumed resting liquidity")
	}
}

func TestSubmitOrderPreservesValidInputForms(t *testing.T) {
	for _, side := range []Side{Bid, Ask} {
		for _, kind := range []OrderType{Limit, Market, StopMarket, StopLimit} {
			for _, status := range []OrderStatus{Pending, Open} {
				name := map[Side]string{Bid: "buy", Ask: "sell"}[side] + map[OrderType]string{Limit: "Limit", Market: "Market", StopMarket: "StopMarket", StopLimit: "StopLimit"}[kind] + map[OrderStatus]string{Pending: "Pending", Open: "Open"}[status]
				t.Run(name, func(t *testing.T) {
					engine := NewMatchingEngine(8, 8)
					defer engine.Close()
					order := testOrder(side, kind, 2)
					order.ClientID = uuid.Nil
					order.Status = status
					if kind == Market || kind == StopMarket {
						order.Price = 0
					}
					original := *order
					submitTestOrder(t, engine, order)
					if *order != original {
						t.Fatal("submission changed valid caller input")
					}
					stats := engine.GetStats()
					if stats.OrdersProcessed != 1 {
						t.Fatalf("valid input was not queued: %+v", stats)
					}
					if kind == StopMarket || kind == StopLimit {
						if stats.PendingStopOrders != 1 {
							t.Fatalf("valid stop was not queued: %+v", stats)
						}
					}
				})
			}
		}
	}
}

func TestSubmitOrderRejectsNilInput(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	defer engine.Close()
	err := engine.SubmitOrder(nil)
	if !errors.Is(err, ErrInvalidOrder) || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("expected nil input validation error; got %v", err)
	}
	if stats := engine.GetStats(); stats != (EngineStats{}) {
		t.Fatalf("nil input changed engine state: %+v", stats)
	}
}

func TestMalformedInputIsRejectedBeforeCommandBackpressure(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	t.Cleanup(engine.Close)
	releaseLoop := holdMatchingLoop(t, engine)
	submitTestOrder(t, engine, testOrder(Ask, Limit, 1))
	invalid := testOrder(Bid, Market, 1)
	invalid.Remaining = 2
	if err := engine.SubmitOrder(invalid); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("expected validation before queue backpressure; got %v", err)
	}
	releaseLoop()
	if stats := engine.GetStats(); stats.OrdersProcessed != 1 || stats.TradesProcessed != 0 {
		t.Fatalf("invalid input entered the full command queue: %+v", stats)
	}
}

func TestMalformedInputIsRejectedBeforeTradeBackpressure(t *testing.T) {
	engine := NewMatchingEngine(8, 0)
	defer engine.Close()
	submitTestOrder(t, engine, testOrder(Ask, Limit, 2))
	submitTestOrder(t, engine, testOrder(Bid, Market, 1))
	synchronizeEngine(t, engine)
	invalid := testOrder(Bid, Market, 1)
	invalid.Symbol = ""
	if err := engine.SubmitOrder(invalid); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("expected validation before trade backpressure; got %v", err)
	}
	trade := readTestTrade(t, engine)
	if trade.Quantity != 1 {
		t.Fatal("invalid input changed an existing trade")
	}
	if stats := engine.GetStats(); stats.OrdersProcessed != 2 || stats.TradesProcessed != 1 {
		t.Fatalf("invalid input affected existing orders: %+v", stats)
	}
}

func TestMalformedInputIsValidatedAfterClose(t *testing.T) {
	engine := NewMatchingEngine(1, 1)
	engine.Close()
	invalid := testOrder(Bid, Limit, 1)
	invalid.Remaining = 0
	if err := engine.SubmitOrder(invalid); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("expected validation before closed state; got %v", err)
	}
	if err := engine.SubmitOrder(testOrder(Bid, Limit, 1)); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("valid order after shutdown returned %v", err)
	}
}

func TestValidationPreservesRepresentableQuantityAndPriceBoundaries(t *testing.T) {
	cases := []struct {
		name                       string
		kind                       OrderType
		quantity, price, stopPrice uint64
	}{
		{"maximumQuantity", Limit, ^uint64(0), 1, 0},
		{"maximumPrice", Limit, 1, ^uint64(0), 0},
		{"maximumStopPrice", StopLimit, 1, 1, ^uint64(0)},
		{"marketIgnoresPrice", Market, 1, ^uint64(0), 0},
		{"marketIgnoresStopPrice", Market, 1, 0, ^uint64(0)},
		{"stopMarketIgnoresPrice", StopMarket, 1, ^uint64(0), 100},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			engine := NewMatchingEngine(4, 4)
			defer engine.Close()
			order := testOrder(Bid, scenario.kind, scenario.quantity)
			order.Price = scenario.price
			order.StopPrice = scenario.stopPrice
			submitTestOrder(t, engine, order)
			if stats := engine.GetStats(); stats.OrdersProcessed != 1 {
				t.Fatalf("valid boundary value was not queued: %+v", stats)
			}
		})
	}
}

func TestValidInputStillUsesAsynchronousRiskChecks(t *testing.T) {
	engine := NewMatchingEngine(4, 4)
	defer engine.Close()
	order := testOrder(Bid, Limit, 2)
	engine.GetRiskManager().SetRiskLimits(order.ClientID, &RiskLimits{MaxOrderSize: 1})
	submitTestOrder(t, engine, order)
	if stats := engine.GetStats(); stats.OrdersProcessed != 1 || stats.ActiveBooks != 0 || stats.TradesProcessed != 0 {
		t.Fatalf("valid order bypassed risk checks: %+v", stats)
	}
	if stats := engine.GetRiskManager().GetStats(); stats.OrdersRejected != 1 {
		t.Fatalf("risk rejection was not preserved: %+v", stats)
	}
}

func TestConcurrentValidationDoesNotChangeAcceptedQuantities(t *testing.T) {
	engine := NewMatchingEngine(256, 8)
	defer engine.Close()
	const count = 64
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			valid := testOrder(Ask, Limit, 1)
			if err := engine.SubmitOrder(valid); err != nil {
				t.Error(err)
				return
			}
			valid.Quantity = 999
			valid.Remaining = 999
			invalid := testOrder(Bid, Market, 1)
			invalid.Remaining = 2
			if err := engine.SubmitOrder(invalid); !errors.Is(err, ErrInvalidOrder) {
				t.Errorf("concurrent malformed input returned %v", err)
			}
		}()
	}
	workers.Wait()
	if stats := engine.GetStats(); stats.OrdersProcessed != count || stats.TradesProcessed != 0 || stats.PendingStopOrders != 0 {
		t.Fatalf("validation changed accepted commands: %+v", stats)
	}
	snapshot := engine.GetSnapshot("TEST")
	if snapshot.BestAsk == nil || snapshot.BestAsk.TotalQty != count || len(snapshot.BestAsk.Orders) != count {
		t.Fatalf("accepted liquidity was changed: %+v", snapshot.BestAsk)
	}
	for _, order := range snapshot.BestAsk.Orders {
		if order.Quantity != 1 || order.Remaining != 1 {
			t.Fatal("validation retained caller-owned order state")
		}
	}
}
