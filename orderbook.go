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

type BidPriceHeap struct {
	PriceHeap
}

func (h BidPriceHeap) Less(i, j int) bool { return h.PriceHeap[i].Price > h.PriceHeap[j].Price }

type OrderBook struct {
	mu        sync.RWMutex
	symbol    string
	bids      BidPriceHeap
	asks      PriceHeap
	topOfBook atomic.Pointer[OrderBookSnapshot]
	orders    map[uuid.UUID]*Order
}

// NewOrderBook creates a new order book for a symbol
func NewOrderBook(symbol string) *OrderBook {
	ob := &OrderBook{
		symbol: symbol,
		bids:   BidPriceHeap{PriceHeap: make(PriceHeap, 0)},
		asks:   make(PriceHeap, 0),
		orders: make(map[uuid.UUID]*Order),
	}
	heap.Init(&ob.bids)
	heap.Init(&ob.asks)
	ob.updateSnapshot()
	return ob
}

// AddOrder adds an order to the appropriate side of the book
func (ob *OrderBook) addOrder(order *Order) {
	ob.orders[order.ID] = order

	if order.Side == Bid {
		ob.addBid(order)
	} else {
		ob.addAsk(order)
	}
	ob.updateSnapshot()
}

// addBid adds a buy order to the bid side
func (ob *OrderBook) addBid(order *Order) {

	// Find existing price level or create new one
	var level *PriceLevel
	for _, l := range ob.bids.PriceHeap {
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
func (ob *OrderBook) getBestBid() *PriceLevel {

	if len(ob.bids.PriceHeap) == 0 {
		return nil
	}
	return ob.bids.PriceHeap[0]
}

// GetBestAsk returns the best ask price level (lowest sell price)
func (ob *OrderBook) getBestAsk() *PriceLevel {

	if len(ob.asks) == 0 {
		return nil
	}
	return ob.asks[0]
}

// GetSnapshot returns a lock-free snapshot of the top of the book
func (ob *OrderBook) GetSnapshot() *OrderBookSnapshot {
	snapshot := ob.topOfBook.Load()
	copied := *snapshot
	copied.BestBid = ob.clonePriceLevel(snapshot.BestBid)
	copied.BestAsk = ob.clonePriceLevel(snapshot.BestAsk)
	return &copied
}

// updateSnapshot atomically updates the top-of-book snapshot
func (ob *OrderBook) updateSnapshot() {
	snapshot := &OrderBookSnapshot{
		Symbol:    ob.symbol,
		BestBid:   ob.clonePriceLevel(ob.getBestBid()),
		BestAsk:   ob.clonePriceLevel(ob.getBestAsk()),
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
	var total uint64
	for i, order := range level.Orders {
		copied := *order
		orders[i] = &copied
		total += copied.Remaining
	}
	return &PriceLevel{
		Price:     level.Price,
		TotalQty:  total,
		Orders:    orders,
		Timestamp: level.Timestamp,
	}
}

// RemoveOrder removes an order from the book
func (ob *OrderBook) removeOrder(orderID uuid.UUID) {
	order, exists := ob.orders[orderID]

	if !exists {
		return
	}

	if order.Side == Bid {
		ob.removeBid(order)
	} else {
		ob.removeAsk(order)
	}

	delete(ob.orders, orderID)

	ob.updateSnapshot()
}

// removeBid removes a buy order from the bid side
func (ob *OrderBook) removeBid(order *Order) {

	for levelIndex, level := range ob.bids.PriceHeap {
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
				heap.Remove(&ob.bids, levelIndex)
			}
			break
		}
	}
}

// removeAsk removes a sell order from the ask side
func (ob *OrderBook) removeAsk(order *Order) {

	for levelIndex, level := range ob.asks {
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
				heap.Remove(&ob.asks, levelIndex)
			}
			break
		}
	}
}

// GetOrder retrieves an order by ID
func (ob *OrderBook) getOrder(orderID uuid.UUID) (*Order, bool) {
	order, exists := ob.orders[orderID]
	return order, exists
}

func (ob *OrderBook) AddOrder(order *Order) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	copied := *order
	ob.addOrder(&copied)
}
func (ob *OrderBook) RemoveOrder(id uuid.UUID) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.removeOrder(id)
}
func (ob *OrderBook) GetOrder(id uuid.UUID) (*Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	order, exists := ob.getOrder(id)
	if !exists {
		return nil, false
	}
	copied := *order
	return &copied, true
}
func (ob *OrderBook) GetBestBid() *PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.clonePriceLevel(ob.getBestBid())
}
func (ob *OrderBook) GetBestAsk() *PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.clonePriceLevel(ob.getBestAsk())
}
