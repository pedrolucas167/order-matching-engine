package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type MatchingEngine struct {
	books       map[string]*OrderBook
	bookMu      sync.RWMutex
	stopOrders  map[string][]*Order
	stopMu      sync.RWMutex
	riskManager *RiskManager
	tradeChan   chan *Trade
	orderChan   chan *Order
	orderPool   *sync.Pool
	tradesProcessed atomic.Uint64
	ordersProcessed atomic.Uint64
}

// NewMatchingEngine creates a new matching engine
func NewMatchingEngine(orderBufferSize, tradeBufferSize int) *MatchingEngine {
	me := &MatchingEngine{
		books:       make(map[string]*OrderBook),
		stopOrders:  make(map[string][]*Order),
		riskManager: NewRiskManager(),
		tradeChan:   make(chan *Trade, tradeBufferSize),
		orderChan:   make(chan *Order, orderBufferSize),
		orderPool: &sync.Pool{
			New: func() interface{} {
				return &Order{}
			},
		},
	}

	// Start the matching loop
	go me.matchingLoop()

	// Start the stop order monitoring loop
	go me.stopOrderMonitor()

	return me
}

// SubmitOrder submits an order to the matching engine
// Returns error if channel is full (backpressure)
func (me *MatchingEngine) SubmitOrder(order *Order) error {
	select {
	case me.orderChan <- order:
		me.ordersProcessed.Add(1)
		return nil
	default:
		// Channel full - backpressure
		return ErrBackpressure
	}
}

// matchingLoop processes orders and performs matching
func (me *MatchingEngine) matchingLoop() {
	for order := range me.orderChan {
		me.processOrder(order)
	}
}

// processOrder handles a single order
func (me *MatchingEngine) processOrder(order *Order) {
	// Perform risk checks before processing
	riskResult := me.riskManager.CheckOrder(order)
	if !riskResult.Approved {
		order.Status = Rejected
		return
	}

	// Get or create order book for symbol
	book := me.getOrCreateBook(order.Symbol)

	// Handle stop orders
	if order.Type == StopMarket || order.Type == StopLimit {
		me.addStopOrder(order)
		return
	}

	// Market orders are matched immediately
	if order.Type == Market {
		me.matchMarketOrder(book, order)
		return
	}

	// Limit orders are added to the book and matched if possible
	me.matchLimitOrder(book, order)
}

// matchMarketOrder matches a market order immediately
func (me *MatchingEngine) matchMarketOrder(book *OrderBook, order *Order) {
	if order.Side == Bid {
		me.matchBidMarket(book, order)
	} else {
		me.matchAskMarket(book, order)
	}

	// If order not fully filled, reject remaining
	if order.Remaining > 0 {
		order.Status = Rejected
	}
}

// matchLimitOrder matches a limit order against the book
func (me *MatchingEngine) matchLimitOrder(book *OrderBook, order *Order) {
	if order.Side == Bid {
		me.matchBidLimit(book, order)
	} else {
		me.matchAskLimit(book, order)
	}

	// If order not fully filled, add to book
	if order.Remaining > 0 {
		order.Status = Open
		if order.Remaining < order.Quantity {
			order.Status = PartialFilled
		}
		book.AddOrder(order)
	} else {
		order.Status = Filled
	}
}

// matchBidLimit matches a buy limit order against asks
func (me *MatchingEngine) matchBidLimit(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		bestAsk := book.GetBestAsk()
		if bestAsk == nil || bestAsk.Price > order.Price {
			break // No matching ask at acceptable price
		}

		// Match against orders at this price level (FIFO)
		for _, askOrder := range bestAsk.Orders {
			if order.Remaining == 0 {
				break
			}

			tradeQty := min(order.Remaining, askOrder.Remaining)
			if tradeQty > 0 {
				// Execute trade
				trade := &Trade{
					ID:        uuid.New(),
					Symbol:    book.symbol,
					Price:     bestAsk.Price,
					Quantity:  tradeQty,
					BidOrder:  order.ID,
					AskOrder:  askOrder.ID,
					BidClient: order.ClientID,
					AskClient: askOrder.ClientID,
					Timestamp: time.Now().UnixNano(),
				}

				// Update order quantities
				order.Remaining -= tradeQty
				askOrder.Remaining -= tradeQty

				// Update ask order status
				if askOrder.Remaining == 0 {
					askOrder.Status = Filled
					book.RemoveOrder(askOrder.ID)
				} else {
					askOrder.Status = PartialFilled
				}

				// Update risk manager with position changes
				me.riskManager.UpdatePosition(trade)

				// Check circuit breaker trigger
				me.riskManager.CheckCircuitBreakerTrigger(book.symbol, bestAsk.Price)

				// Publish trade
				me.publishTrade(trade)
			}
		}
	}
}

// matchAskLimit matches a sell limit order against bids
func (me *MatchingEngine) matchAskLimit(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		bestBid := book.GetBestBid()
		if bestBid == nil || bestBid.Price < order.Price {
			break // No matching bid at acceptable price
		}

		// Match against orders at this price level (FIFO)
		for _, bidOrder := range bestBid.Orders {
			if order.Remaining == 0 {
				break
			}

			tradeQty := min(order.Remaining, bidOrder.Remaining)
			if tradeQty > 0 {
				// Execute trade
				trade := &Trade{
					ID:        uuid.New(),
					Symbol:    book.symbol,
					Price:     bestBid.Price,
					Quantity:  tradeQty,
					BidOrder:  bidOrder.ID,
					AskOrder:  order.ID,
					BidClient: bidOrder.ClientID,
					AskClient: order.ClientID,
					Timestamp: time.Now().UnixNano(),
				}

				// Update order quantities
				order.Remaining -= tradeQty
				bidOrder.Remaining -= tradeQty

				// Update bid order status
				if bidOrder.Remaining == 0 {
					bidOrder.Status = Filled
					book.RemoveOrder(bidOrder.ID)
				} else {
					bidOrder.Status = PartialFilled
				}

				// Update risk manager with position changes
				me.riskManager.UpdatePosition(trade)

				// Check circuit breaker trigger
				me.riskManager.CheckCircuitBreakerTrigger(book.symbol, bestBid.Price)

				// Publish trade
				me.publishTrade(trade)
			}
		}
	}
}

// matchBidMarket matches a buy market order against asks
func (me *MatchingEngine) matchBidMarket(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		bestAsk := book.GetBestAsk()
		if bestAsk == nil {
			break // No asks available
		}

		// Match against orders at this price level (FIFO)
		for _, askOrder := range bestAsk.Orders {
			if order.Remaining == 0 {
				break
			}

			tradeQty := min(order.Remaining, askOrder.Remaining)
			if tradeQty > 0 {
				trade := &Trade{
					ID:        uuid.New(),
					Symbol:    book.symbol,
					Price:     bestAsk.Price,
					Quantity:  tradeQty,
					BidOrder:  order.ID,
					AskOrder:  askOrder.ID,
					BidClient: order.ClientID,
					AskClient: askOrder.ClientID,
					Timestamp: time.Now().UnixNano(),
				}

				order.Remaining -= tradeQty
				askOrder.Remaining -= tradeQty

				if askOrder.Remaining == 0 {
					askOrder.Status = Filled
					book.RemoveOrder(askOrder.ID)
				} else {
					askOrder.Status = PartialFilled
				}

				me.riskManager.UpdatePosition(trade)
				me.riskManager.CheckCircuitBreakerTrigger(book.symbol, bestAsk.Price)
				me.publishTrade(trade)
			}
		}
	}
}

// matchAskMarket matches a sell market order against bids
func (me *MatchingEngine) matchAskMarket(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		bestBid := book.GetBestBid()
		if bestBid == nil {
			break // No bids available
		}

		// Match against orders at this price level (FIFO)
		for _, bidOrder := range bestBid.Orders {
			if order.Remaining == 0 {
				break
			}

			tradeQty := min(order.Remaining, bidOrder.Remaining)
			if tradeQty > 0 {
				trade := &Trade{
					ID:        uuid.New(),
					Symbol:    book.symbol,
					Price:     bestBid.Price,
					Quantity:  tradeQty,
					BidOrder:  bidOrder.ID,
					AskOrder:  order.ID,
					BidClient: bidOrder.ClientID,
					AskClient: order.ClientID,
					Timestamp: time.Now().UnixNano(),
				}

				order.Remaining -= tradeQty
				bidOrder.Remaining -= tradeQty

				if bidOrder.Remaining == 0 {
					bidOrder.Status = Filled
					book.RemoveOrder(bidOrder.ID)
				} else {
					bidOrder.Status = PartialFilled
				}

				me.riskManager.UpdatePosition(trade)
				me.riskManager.CheckCircuitBreakerTrigger(book.symbol, bestBid.Price)
				me.publishTrade(trade)
			}
		}
	}
}

// publishTrade sends a trade to the publication channel
func (me *MatchingEngine) publishTrade(trade *Trade) {
	me.tradesProcessed.Add(1)
	select {
	case me.tradeChan <- trade:
		// Trade published successfully
	default:
		// Trade channel full - in production, this would trigger backpressure handling
		// For now, we'll block briefly to avoid losing trades
		me.tradeChan <- trade
	}
}

// getOrCreateBook gets or creates an order book for a symbol
func (me *MatchingEngine) getOrCreateBook(symbol string) *OrderBook {
	me.bookMu.RLock()
	book, exists := me.books[symbol]
	me.bookMu.RUnlock()

	if exists {
		return book
	}

	me.bookMu.Lock()
	defer me.bookMu.Unlock()

	// Double-check after acquiring write lock
	if book, exists := me.books[symbol]; exists {
		return book
	}

	book = NewOrderBook(symbol)
	me.books[symbol] = book
	return book
}

// GetTradeChannel returns the trade publication channel
func (me *MatchingEngine) GetTradeChannel() <-chan *Trade {
	return me.tradeChan
}

// GetOrderBook returns the order book for a symbol
func (me *MatchingEngine) GetOrderBook(symbol string) *OrderBook {
	me.bookMu.RLock()
	defer me.bookMu.RUnlock()
	return me.books[symbol]
}

// GetSnapshot returns a lock-free snapshot of the top of the book
func (me *MatchingEngine) GetSnapshot(symbol string) *OrderBookSnapshot {
	book := me.GetOrderBook(symbol)
	if book == nil {
		return nil
	}
	return book.GetSnapshot()
}

// CancelOrder cancels an existing order
func (me *MatchingEngine) CancelOrder(orderID uuid.UUID, symbol string) bool {
	book := me.GetOrderBook(symbol)
	if book == nil {
		return false
	}

	order, exists := book.GetOrder(orderID)
	if !exists {
		return false
	}

	order.Status = Cancelled
	book.RemoveOrder(orderID)
	return true
}

// GetStats returns engine statistics
func (me *MatchingEngine) GetStats() EngineStats {
	return EngineStats{
		TradesProcessed:  me.tradesProcessed.Load(),
		OrdersProcessed: me.ordersProcessed.Load(),
		ActiveBooks:     uint64(len(me.books)),
		PendingStopOrders: me.countStopOrders(),
	}
}

// GetRiskManager returns the risk manager
func (me *MatchingEngine) GetRiskManager() *RiskManager {
	return me.riskManager
}

// EngineStats represents matching engine statistics
type EngineStats struct {
	TradesProcessed   uint64
	OrdersProcessed  uint64
	ActiveBooks      uint64
	PendingStopOrders uint64
}

// addStopOrder adds a stop order to the pending list
func (me *MatchingEngine) addStopOrder(order *Order) {
	order.Status = Pending

	me.stopMu.Lock()
	defer me.stopMu.Unlock()

	me.stopOrders[order.Symbol] = append(me.stopOrders[order.Symbol], order)
}

// stopOrderMonitor continuously checks if stop orders should be triggered
func (me *MatchingEngine) stopOrderMonitor() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		me.checkAndTriggerStopOrders()
	}
}

// checkAndTriggerStopOrders checks all pending stop orders and triggers those that should execute
func (me *MatchingEngine) checkAndTriggerStopOrders() {
	me.stopMu.Lock()
	symbols := make([]string, 0, len(me.stopOrders))
	for symbol := range me.stopOrders {
		symbols = append(symbols, symbol)
	}
	me.stopMu.Unlock()

	for _, symbol := range symbols {
		me.processStopOrdersForSymbol(symbol)
	}
}

// processStopOrdersForSymbol processes stop orders for a specific symbol
func (me *MatchingEngine) processStopOrdersForSymbol(symbol string) {
	book := me.GetOrderBook(symbol)
	if book == nil {
		return
	}

	bestBid := book.GetBestBid()
	bestAsk := book.GetBestAsk()

	var currentPrice uint64
	if bestBid != nil && bestAsk != nil {
		currentPrice = (bestBid.Price + bestAsk.Price) / 2
	} else if bestBid != nil {
		currentPrice = bestBid.Price
	} else if bestAsk != nil {
		currentPrice = bestAsk.Price
	} else {
		return // No market data available
	}

	me.stopMu.Lock()
	defer me.stopMu.Unlock()

	orders := me.stopOrders[symbol]
	var remainingOrders []*Order

	for _, order := range orders {
		if order.Status == Cancelled {
			continue
		}

		shouldTrigger := false

		// Stop-Loss for sell orders: trigger if price drops below stop price
		if order.Side == Ask && currentPrice <= order.StopPrice {
			shouldTrigger = true
		}

		// Stop-Loss for buy orders: trigger if price rises above stop price
		if order.Side == Bid && currentPrice >= order.StopPrice {
			shouldTrigger = true
		}

		if shouldTrigger {
			me.triggerStopOrder(order)
		} else {
			remainingOrders = append(remainingOrders, order)
		}
	}

	me.stopOrders[symbol] = remainingOrders
}

// triggerStopOrder converts a stop order to a market or limit order
func (me *MatchingEngine) triggerStopOrder(stopOrder *Order) {
	stopOrder.Status = Triggered

	// Create the actual order to execute
	var execOrder *Order

	if stopOrder.Type == StopMarket {
		execOrder = &Order{
			ID:        stopOrder.ID,
			Symbol:    stopOrder.Symbol,
			Side:      stopOrder.Side,
			Type:      Market,
			Price:     0,
			Quantity:  stopOrder.Quantity,
			Remaining: stopOrder.Remaining,
			Status:    Open,
			Timestamp: time.Now().UnixNano(),
		}
	} else { // StopLimit
		execOrder = &Order{
			ID:        stopOrder.ID,
			Symbol:    stopOrder.Symbol,
			Side:      stopOrder.Side,
			Type:      Limit,
			Price:     stopOrder.Price, // Use limit price
			Quantity:  stopOrder.Quantity,
			Remaining: stopOrder.Remaining,
			Status:    Open,
			Timestamp: time.Now().UnixNano(),
		}
	}

	// Submit the triggered order for execution
	book := me.getOrCreateBook(stopOrder.Symbol)
	if execOrder.Type == Market {
		me.matchMarketOrder(book, execOrder)
	} else {
		me.matchLimitOrder(book, execOrder)
	}

	// Update original stop order status
	if execOrder.Remaining == 0 {
		stopOrder.Status = Filled
	} else if execOrder.Remaining < stopOrder.Quantity {
		stopOrder.Status = PartialFilled
		stopOrder.Remaining = execOrder.Remaining
	}
}

// countStopOrders returns the total number of pending stop orders
func (me *MatchingEngine) countStopOrders() uint64 {
	me.stopMu.RLock()
	defer me.stopMu.RUnlock()

	count := uint64(0)
	for _, orders := range me.stopOrders {
		count += uint64(len(orders))
	}
	return count
}

// CancelStopOrder cancels a pending stop order
func (me *MatchingEngine) CancelStopOrder(orderID uuid.UUID, symbol string) bool {
	me.stopMu.Lock()
	defer me.stopMu.Unlock()

	orders := me.stopOrders[symbol]
	for i, order := range orders {
		if order.ID == orderID && order.Status == Pending {
			order.Status = Cancelled
			// Remove from list
			me.stopOrders[symbol] = append(orders[:i], orders[i+1:]...)
			return true
		}
	}
	return false
}

// ErrBackpressure is returned when the order channel is full
var ErrBackpressure = errors.New("order channel full - backpressure")

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
