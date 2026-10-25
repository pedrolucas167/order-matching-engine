package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var ErrInvalidOrder = errors.New("invalid order")

func validateNewOrder(order Order) error {
	if order.ID == uuid.Nil {
		return fmt.Errorf("%w: order ID is required", ErrInvalidOrder)
	}
	symbol := strings.TrimSpace(order.Symbol)
	if symbol == "" {
		return fmt.Errorf("%w: symbol is required", ErrInvalidOrder)
	}
	if symbol != order.Symbol {
		return fmt.Errorf("%w: symbol must not have leading or trailing whitespace", ErrInvalidOrder)
	}
	if order.Side != Bid && order.Side != Ask {
		return fmt.Errorf("%w: side must be Bid or Ask", ErrInvalidOrder)
	}
	switch order.Type {
	case Limit, Market, StopMarket, StopLimit:
	default:
		return fmt.Errorf("%w: unsupported order type", ErrInvalidOrder)
	}
	if order.Quantity == 0 {
		return fmt.Errorf("%w: quantity must be greater than zero", ErrInvalidOrder)
	}
	if order.Remaining != order.Quantity {
		return fmt.Errorf("%w: remaining must equal quantity for a new order", ErrInvalidOrder)
	}
	if order.Status != Pending && order.Status != Open {
		return fmt.Errorf("%w: initial status must be Pending or Open", ErrInvalidOrder)
	}
	if (order.Type == Limit || order.Type == StopLimit) && order.Price == 0 {
		return fmt.Errorf("%w: price must be greater than zero for limit orders", ErrInvalidOrder)
	}
	if (order.Type == StopMarket || order.Type == StopLimit) && order.StopPrice == 0 {
		return fmt.Errorf("%w: stop price must be greater than zero for stop orders", ErrInvalidOrder)
	}
	return nil
}
