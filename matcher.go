package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type MatchingEngine struct {
	books             map[string]*OrderBook
	stopOrders        map[string][]*Order
	riskManager       *RiskManager
	tradeChan         chan *Trade
	commandChan       chan func()
	stop              chan struct{}
	done              chan struct{}
	closeOnce         sync.Once
	pendingTrades     []*Trade
	pendingTradeCount atomic.Int64
	tradeBacklogLimit int64
	tradesProcessed   atomic.Uint64
	ordersProcessed   atomic.Uint64
}

func NewMatchingEngine(orderBufferSize, tradeBufferSize int) *MatchingEngine {
	me := &MatchingEngine{
		books:       make(map[string]*OrderBook),
		stopOrders:  make(map[string][]*Order),
		riskManager: NewRiskManager(),
		tradeChan:   make(chan *Trade, tradeBufferSize),
		commandChan: make(chan func(), orderBufferSize),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	me.tradeBacklogLimit = int64(tradeBufferSize)
	if me.tradeBacklogLimit < 1 {
		me.tradeBacklogLimit = 1
	}
	go me.matchingLoop()
	return me
}

func (me *MatchingEngine) SubmitOrder(order *Order) error {
	if order == nil {
		return errors.New("nil order")
	}
	select {
	case <-me.stop:
		return ErrEngineClosed
	default:
	}
	if me.pendingTradeCount.Load() >= me.tradeBacklogLimit {
		return ErrBackpressure
	}
	copiedOrder := *order
	select {
	case <-me.stop:
		return ErrEngineClosed
	case me.commandChan <- func() { me.processOrder(&copiedOrder) }:
		me.ordersProcessed.Add(1)
		return nil
	default:
		return ErrBackpressure
	}
}

func (me *MatchingEngine) runCommand(command func()) bool {
	completed := make(chan struct{})
	select {
	case <-me.stop:
		return false
	case me.commandChan <- func() { command(); close(completed) }:
	}
	select {
	case <-completed:
		return true
	case <-me.done:
		return false
	}
}

func (me *MatchingEngine) Close() {
	me.closeOnce.Do(func() { close(me.stop) })
	<-me.done
}

func (me *MatchingEngine) matchingLoop() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	defer close(me.done)
	defer close(me.tradeChan)
	defer me.riskManager.Close()
	for {
		select {
		case <-me.stop:
			return
		default:
		}
		var output chan *Trade
		var next *Trade
		if len(me.pendingTrades) > 0 {
			output = me.tradeChan
			next = me.pendingTrades[0]
		}
		select {
		case <-me.stop:
			return
		case command := <-me.commandChan:
			command()
		case <-ticker.C:
			me.checkAndTriggerStopOrders()
		case output <- next:
			me.pendingTrades[0] = nil
			me.pendingTrades = me.pendingTrades[1:]
			me.pendingTradeCount.Add(-1)
		}
	}
}

// processOrder handles a single order
func (me *MatchingEngine) processOrder(order *Order) {
	if me.hasActiveOrder(order.ID, order.Symbol) {
		order.Status = Rejected
		return
	}
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

func (me *MatchingEngine) hasActiveOrder(orderID uuid.UUID, symbol string) bool {
	if book := me.books[symbol]; book != nil {
		if _, exists := book.orders[orderID]; exists {
			return true
		}
	}
	for _, order := range me.stopOrders[symbol] {
		if order.ID == orderID {
			return true
		}
	}
	return false
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
	} else {
		order.Status = Filled
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
		book.addOrder(order)
	} else {
		order.Status = Filled
	}
}

// matchBidLimit matches a buy limit order against asks
func (me *MatchingEngine) matchBidLimit(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		bestAsk := book.getBestAsk()
		if bestAsk == nil || bestAsk.Price > order.Price {
			break // No matching ask at acceptable price
		}

		// Match against orders at this price level (FIFO)
		for _, askOrder := range append([]*Order(nil), bestAsk.Orders...) {
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
				bestAsk.TotalQty -= tradeQty

				// Update ask order status
				if askOrder.Remaining == 0 {
					askOrder.Status = Filled
					book.removeOrder(askOrder.ID)
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
		bestBid := book.getBestBid()
		if bestBid == nil || bestBid.Price < order.Price {
			break // No matching bid at acceptable price
		}

		// Match against orders at this price level (FIFO)
		for _, bidOrder := range append([]*Order(nil), bestBid.Orders...) {
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
				bestBid.TotalQty -= tradeQty

				// Update bid order status
				if bidOrder.Remaining == 0 {
					bidOrder.Status = Filled
					book.removeOrder(bidOrder.ID)
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
		bestAsk := book.getBestAsk()
		if bestAsk == nil {
			break // No asks available
		}

		// Match against orders at this price level (FIFO)
		for _, askOrder := range append([]*Order(nil), bestAsk.Orders...) {
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
				bestAsk.TotalQty -= tradeQty

				if askOrder.Remaining == 0 {
					askOrder.Status = Filled
					book.removeOrder(askOrder.ID)
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
		bestBid := book.getBestBid()
		if bestBid == nil {
			break // No bids available
		}

		// Match against orders at this price level (FIFO)
		for _, bidOrder := range append([]*Order(nil), bestBid.Orders...) {
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
				bestBid.TotalQty -= tradeQty

				if bidOrder.Remaining == 0 {
					bidOrder.Status = Filled
					book.removeOrder(bidOrder.ID)
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
	me.pendingTrades = append(me.pendingTrades, trade)
	me.pendingTradeCount.Add(1)
}

// getOrCreateBook gets or creates an order book for a symbol
func (me *MatchingEngine) getOrCreateBook(symbol string) *OrderBook {
	book := me.books[symbol]
	if book == nil {
		book = NewOrderBook(symbol)
		me.books[symbol] = book
	}
	return book
}

// GetTradeChannel returns the trade publication channel
func (me *MatchingEngine) GetTradeChannel() <-chan *Trade {
	return me.tradeChan
}

// GetOrderBook returns the order book for a symbol
func (me *MatchingEngine) GetOrderBook(symbol string) *OrderBook {
	var bookCopy *OrderBook
	me.runCommand(func() {
		if book := me.books[symbol]; book != nil {
			bookCopy = NewOrderBook(symbol)
			for _, levels := range []PriceHeap{book.bids.PriceHeap, book.asks} {
				for _, level := range levels {
					for _, order := range level.Orders {
						bookCopy.AddOrder(order)
					}
				}
			}
		}
	})
	return bookCopy
}

func (me *MatchingEngine) GetSnapshot(symbol string) *OrderBookSnapshot {
	var snapshot *OrderBookSnapshot
	me.runCommand(func() {
		if book := me.books[symbol]; book != nil {
			book.updateSnapshot()
			snapshot = book.GetSnapshot()
		}
	})
	return snapshot
}

func (me *MatchingEngine) CancelOrder(orderID uuid.UUID, symbol string) bool {
	cancelled := false
	me.runCommand(func() {
		if book := me.books[symbol]; book != nil {
			if order, exists := book.orders[orderID]; exists {
				order.Status = Cancelled
				book.removeOrder(orderID)
				cancelled = true
			}
		}
	})
	return cancelled
}

func (me *MatchingEngine) GetStats() EngineStats {
	stats := EngineStats{}
	me.runCommand(func() {
		stats.TradesProcessed = me.tradesProcessed.Load()
		stats.OrdersProcessed = me.ordersProcessed.Load()
		stats.ActiveBooks = uint64(len(me.books))
		stats.PendingStopOrders = me.countStopOrders()
	})
	return stats
}

// GetRiskManager returns the risk manager
func (me *MatchingEngine) GetRiskManager() *RiskManager {
	return me.riskManager
}

// EngineStats represents matching engine statistics
type EngineStats struct {
	TradesProcessed   uint64
	OrdersProcessed   uint64
	ActiveBooks       uint64
	PendingStopOrders uint64
}

// addStopOrder adds a stop order to the pending list
func (me *MatchingEngine) addStopOrder(order *Order) {
	order.Status = Pending

	me.stopOrders[order.Symbol] = append(me.stopOrders[order.Symbol], order)
}

// checkAndTriggerStopOrders checks all pending stop orders and triggers those that should execute
func (me *MatchingEngine) checkAndTriggerStopOrders() {
	symbols := make([]string, 0, len(me.stopOrders))
	for symbol := range me.stopOrders {
		symbols = append(symbols, symbol)
	}

	for _, symbol := range symbols {
		me.processStopOrdersForSymbol(symbol)
	}
}

// processStopOrdersForSymbol processes stop orders for a specific symbol
func (me *MatchingEngine) processStopOrdersForSymbol(symbol string) {
	book := me.books[symbol]
	if book == nil {
		return
	}

	bestBid := book.getBestBid()
	bestAsk := book.getBestAsk()

	var currentPrice uint64
	if bestBid != nil && bestAsk != nil {
		currentPrice = bestBid.Price + (bestAsk.Price-bestBid.Price)/2
	} else if bestBid != nil {
		currentPrice = bestBid.Price
	} else if bestAsk != nil {
		currentPrice = bestAsk.Price
	} else {
		return // No market data available
	}

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

func (me *MatchingEngine) triggerStopOrder(order *Order) {
	order.Timestamp = time.Now().UnixNano()
	book := me.getOrCreateBook(order.Symbol)
	if order.Type == StopMarket {
		order.Type = Market
		order.Price = 0
		me.matchMarketOrder(book, order)
	} else {
		order.Type = Limit
		me.matchLimitOrder(book, order)
	}
}

// countStopOrders returns the total number of pending stop orders
func (me *MatchingEngine) countStopOrders() uint64 {

	count := uint64(0)
	for _, orders := range me.stopOrders {
		count += uint64(len(orders))
	}
	return count
}

// CancelStopOrder cancels a pending stop order
func (me *MatchingEngine) cancelStopOrder(orderID uuid.UUID, symbol string) bool {

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

var ErrEngineClosed = errors.New("matching engine closed")

func (me *MatchingEngine) CancelStopOrder(orderID uuid.UUID, symbol string) bool {
	cancelled := false
	me.runCommand(func() { cancelled = me.cancelStopOrder(orderID, symbol) })
	return cancelled
}
