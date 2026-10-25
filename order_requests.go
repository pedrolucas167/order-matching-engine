package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrInvalidOrderRequest   = errors.New("request requires a context and an ASCII key of 1 to 128 non-whitespace characters")
	ErrRequestConflict       = errors.New("idempotency key is already bound to different order data")
	ErrOrderIDConflict       = errors.New("order ID was already submitted")
	ErrRequestCapacity       = errors.New("idempotency record capacity reached")
	ErrRequestOutcomeUnknown = errors.New("request outcome is not confirmed; retry with the same key and order")
	ErrOrderRejected         = errors.New("order rejected or its remainder could not be filled")
	ErrPriceUnavailable      = errors.New("required price feed is unavailable")
)

type OrderReceipt struct {
	Sequence          uint64    `json:"sequence"`
	OrderID           uuid.UUID `json:"order_id"`
	Symbol            string    `json:"symbol"`
	InitialStatus     string    `json:"initial_status"`
	FilledQuantity    uint64    `json:"filled_quantity,string"`
	RemainingQuantity uint64    `json:"remaining_quantity,string"`
	RejectionReason   string    `json:"rejection_reason,omitempty"`
}

type processedOrderRequest struct {
	order   Order
	receipt OrderReceipt
}

type orderRequestResult struct {
	receipt  OrderReceipt
	replayed bool
	err      error
}

func validRequestKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

func (me *MatchingEngine) SetOrderRequestCapacity(limit int) error {
	if limit < 1 {
		return ErrRequestCapacity
	}
	var result error
	if !me.runCommand(func() {
		if limit < len(me.requests) {
			result = ErrRequestCapacity
			return
		}
		me.requestLimit = limit
	}) {
		return ErrEngineClosed
	}
	return result
}

func (me *MatchingEngine) SubmitOrderRequest(ctx context.Context, key string, order *Order) (OrderReceipt, bool, error) {
	if ctx == nil || !validRequestKey(key) {
		return OrderReceipt{}, false, ErrInvalidOrderRequest
	}
	if order == nil {
		return OrderReceipt{}, false, ErrInvalidOrder
	}
	copied := *order
	if err := validateNewOrder(copied); err != nil {
		return OrderReceipt{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return OrderReceipt{}, false, err
	}
	select {
	case <-me.stop:
		return OrderReceipt{}, false, ErrEngineClosed
	default:
	}
	completed := make(chan orderRequestResult, 1)
	select {
	case <-me.stop:
		return OrderReceipt{}, false, ErrEngineClosed
	case <-ctx.Done():
		return OrderReceipt{}, false, ctx.Err()
	case me.commandChan <- func() { completed <- me.processOrderRequest(key, copied) }:
	default:
		return OrderReceipt{}, false, ErrBackpressure
	}
	select {
	case result := <-completed:
		return result.receipt, result.replayed, result.err
	case <-ctx.Done():
		return OrderReceipt{}, false, fmt.Errorf("%w: %w", ErrRequestOutcomeUnknown, ctx.Err())
	case <-me.done:
		select {
		case result := <-completed:
			return result.receipt, result.replayed, result.err
		default:
		}
		return OrderReceipt{}, false, fmt.Errorf("%w: %w", ErrRequestOutcomeUnknown, ErrEngineClosed)
	}
}

func (me *MatchingEngine) processOrderRequest(key string, order Order) orderRequestResult {
	if previous, exists := me.requests[key]; exists {
		if previous.order != order {
			return orderRequestResult{err: ErrRequestConflict}
		}
		return orderRequestResult{receipt: previous.receipt, replayed: true}
	}
	if _, exists := me.requestOrders[order.ID]; exists {
		return orderRequestResult{err: ErrOrderIDConflict}
	}
	if me.hasActiveOrder(order.ID, order.Symbol) {
		return orderRequestResult{err: ErrOrderIDConflict}
	}
	if len(me.requests) >= me.requestLimit {
		return orderRequestResult{err: ErrRequestCapacity}
	}
	if me.pendingTradeCount.Load() >= me.tradeBacklogLimit {
		return orderRequestResult{err: ErrBackpressure}
	}
	original := order
	err := me.processOrder(&order)
	me.ordersProcessed.Add(1)
	me.requestSequence++
	receipt := OrderReceipt{Sequence: me.requestSequence, OrderID: order.ID, Symbol: order.Symbol, InitialStatus: orderStatusName(order.Status), FilledQuantity: order.Quantity - order.Remaining, RemainingQuantity: order.Remaining}
	if err != nil {
		receipt.RejectionReason = err.Error()
	}
	me.requests[key] = processedOrderRequest{order: original, receipt: receipt}
	me.requestOrders[order.ID] = key
	return orderRequestResult{receipt: receipt}
}

func orderStatusName(status OrderStatus) string {
	names := [...]string{"pending", "open", "partially_filled", "filled", "cancelled", "rejected", "triggered"}
	if status < Pending || status > Triggered {
		return "unknown"
	}
	return names[status]
}
