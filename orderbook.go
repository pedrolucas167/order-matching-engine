package main

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// PriceHeap implements heap.Interface for price levels
type PriceHeap []*PriceLevel

func (h PriceHeap) Len() int           { return len(h) }
func (h PriceHeap) Less(i, j int) bool { return h[i].Price < h[j].Price }
func (h PriceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *PriceHeap) Push(x interface{}) {
	*h = append(*h, x.(*PriceLevel))
}

func (h *PriceHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type OrderBook struct {
	symbol string
	bids  PriceHeap
	bidMu sync.RWMutex
	asks  PriceHeap
	askMu sync.RWMutex
	topOfBook atomic.Pointer[OrderBookSnapshot]
	orders map[uuid.UUID]*Order
	orderMu sync.RWMutex
}

// NewOrderBook creates a new order book for a symbol
func NewOrderBook(symbol string) *OrderBook {
	ob := &OrderBook{
		symbol:  symbol,
		bids:    make(PriceHeap, 0),
		asks:    make(PriceHeap, 0),
		orders:  make(map[uuid.UUID]*Order),
	}
	heap.Init(&ob.bids)
	heap.Init(&ob.asks)
	ob.updateSnapshot()
	return ob
}

// AddOrder adds an order to the appropriate side of the book
func (ob *OrderBook) AddOrder(order *Order) {
	ob.orderMu.Lock()
	ob.orders[order.ID] = order
	ob.orderMu.Unlock()

	if order.Side == Bid {
		ob.addBid(order)
	} else {
		ob.addAsk(order)
	}
	ob.updateSnapshot()
}

// addBid adds a buy order to the bid side
func (ob *OrderBook) addBid(order *Order) {
	ob.bidMu.Lock()
	defer ob.bidMu.Unlock()

	// Find existing price level or create new one
	var level *PriceLevel
	for _, l := range ob.bids {
		if l.Price == order.Price {
			level = l
			break
		}
	}

	if level == nil {
		level = &PriceLevel{
			Price:     order.Price,
			TotalQty:  order.Quantity,
			Orders:    []*Order{order},
			Timestamp: order.Timestamp,
		}
		heap.Push(&ob.bids, level)
	} else {
		level.TotalQty += order.Quantity
		level.Orders = append(level.Orders, order)
	}
}

// addAsk adds a sell order to the ask side
func (ob *OrderBook) addAsk(order *Order) {
	ob.askMu.Lock()
	defer ob.askMu.Unlock()

	// Find existing price level or create new one
	var level *PriceLevel
	for _, l := range ob.asks {
		if l.Price == order.Price {
			level = l
			break
		}
	}

	if level == nil {
		level = &PriceLevel{
			Price:     order.Price,
			TotalQty:  order.Quantity,
			Orders:    []*Order{order},
			Timestamp: order.Timestamp,
		}
		heap.Push(&ob.asks, level)
	} else {
		level.TotalQty += order.Quantity
		level.Orders = append(level.Orders, order)
	}
}

// GetBestBid returns the best bid price level (highest buy price)
func (ob *OrderBook) GetBestBid() *PriceLevel {
	ob.bidMu.RLock()
	defer ob.bidMu.RUnlock()

	if len(ob.bids) == 0 {
		return nil
	}
	return ob.bids[0]
}

// GetBestAsk returns the best ask price level (lowest sell price)
func (ob *OrderBook) GetBestAsk() *PriceLevel {
	ob.askMu.RLock()
	defer ob.askMu.RUnlock()

	if len(ob.asks) == 0 {
		return nil
	}
	return ob.asks[0]
}

// GetSnapshot returns a lock-free snapshot of the top of the book
func (ob *OrderBook) GetSnapshot() *OrderBookSnapshot {
	return ob.topOfBook.Load()
}

// updateSnapshot atomically updates the top-of-book snapshot
func (ob *OrderBook) updateSnapshot() {
	snapshot := &OrderBookSnapshot{
		Symbol:    ob.symbol,
		BestBid:   ob.clonePriceLevel(ob.GetBestBid()),
		BestAsk:   ob.clonePriceLevel(ob.GetBestAsk()),
		Timestamp: time.Now().UnixNano(),
	}
	ob.topOfBook.Store(snapshot)
}

// clonePriceLevel creates a deep copy of a price level for snapshot
func (ob *OrderBook) clonePriceLevel(level *PriceLevel) *PriceLevel {
	if level == nil {
		return nil
	}
	orders := make([]*Order, len(level.Orders))
	copy(orders, level.Orders)
	return &PriceLevel{
		Price:     level.Price,
		TotalQty:  level.TotalQty,
		Orders:    orders,
		Timestamp: level.Timestamp,
	}
}

// RemoveOrder removes an order from the book
func (ob *OrderBook) RemoveOrder(orderID uuid.UUID) {
	ob.orderMu.RLock()
	order, exists := ob.orders[orderID]
	ob.orderMu.RUnlock()

	if !exists {
		return
	}

	if order.Side == Bid {
		ob.removeBid(order)
	} else {
		ob.removeAsk(order)
	}

	ob.orderMu.Lock()
	delete(ob.orders, orderID)
	ob.orderMu.Unlock()

	ob.updateSnapshot()
}

// removeBid removes a buy order from the bid side
func (ob *OrderBook) removeBid(order *Order) {
	ob.bidMu.Lock()
	defer ob.bidMu.Unlock()

	for _, level := range ob.bids {
		if level.Price == order.Price {
			for i, o := range level.Orders {
				if o.ID == order.ID {
					level.Orders = append(level.Orders[:i], level.Orders[i+1:]...)
					level.TotalQty -= order.Remaining
					break
				}
			}
			// Remove empty price level
			if len(level.Orders) == 0 {
				ob.removePriceLevel(&ob.bids, level.Price)
			}
			break
		}
	}
}

// removeAsk removes a sell order from the ask side
func (ob *OrderBook) removeAsk(order *Order) {
	ob.askMu.Lock()
	defer ob.askMu.Unlock()

	for _, level := range ob.asks {
		if level.Price == order.Price {
			for i, o := range level.Orders {
				if o.ID == order.ID {
					level.Orders = append(level.Orders[:i], level.Orders[i+1:]...)
					level.TotalQty -= order.Remaining
					break
				}
			}
			// Remove empty price level
			if len(level.Orders) == 0 {
				ob.removePriceLevel(&ob.asks, level.Price)
			}
			break
		}
	}
}

// removePriceLevel removes a price level from the heap
func (ob *OrderBook) removePriceLevel(h *PriceHeap, price uint64) {
	for i, level := range *h {
		if level.Price == price {
			heap.Remove(h, i)
			break
		}
	}
}

// GetOrder retrieves an order by ID
func (ob *OrderBook) GetOrder(orderID uuid.UUID) (*Order, bool) {
	ob.orderMu.RLock()
	defer ob.orderMu.RUnlock()
	order, exists := ob.orders[orderID]
	return order, exists
}
