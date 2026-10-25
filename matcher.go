package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type MatchingEngine struct {
	books              map[string]*OrderBook
	stopOrders         map[string][]*Order
	riskManager        *RiskManager
	tradeChan          chan *Trade
	commandChan        chan func()
	stop               chan struct{}
	done               chan struct{}
	closeOnce          sync.Once
	pendingTrades      []*Trade
	pendingTradeCount  atomic.Int64
	tradeBacklogLimit  int64
	tradesProcessed    atomic.Uint64
	ordersProcessed    atomic.Uint64
	priceFeeds         map[string]*priceFeedState
	markTimes          map[string]time.Time
	liquidationPolicy  LiquidationPolicy
	liquidations       map[liquidationKey]LiquidationStatus
	requests           map[string]processedOrderRequest
	requestOrders      map[uuid.UUID]string
	requestLimit       int
	requestSequence    uint64
	managedPersistence bool
}

func NewMatchingEngine(orderBufferSize, tradeBufferSize int) *MatchingEngine {
	return newMatchingEngine(orderBufferSize, tradeBufferSize, false)
}

func NewDerivativesMatchingEngine(orderBufferSize, tradeBufferSize int) *MatchingEngine {
	return newMatchingEngine(orderBufferSize, tradeBufferSize, true)
}

func newMatchingEngine(orderBufferSize, tradeBufferSize int, derivativesEnabled bool) *MatchingEngine {
	return newEngineMode(orderBufferSize, tradeBufferSize, derivativesEnabled, false)
}

func newEngineMode(orderBufferSize, tradeBufferSize int, derivativesEnabled, managed bool) *MatchingEngine {
	me := &MatchingEngine{
		books:              make(map[string]*OrderBook),
		stopOrders:         make(map[string][]*Order),
		riskManager:        newRiskManagerMode(derivativesEnabled, !managed),
		tradeChan:          make(chan *Trade, tradeBufferSize),
		commandChan:        make(chan func(), orderBufferSize),
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
		priceFeeds:         make(map[string]*priceFeedState),
		markTimes:          make(map[string]time.Time),
		liquidations:       make(map[liquidationKey]LiquidationStatus),
		requests:           make(map[string]processedOrderRequest),
		requestOrders:      make(map[uuid.UUID]string),
		requestLimit:       100000,
		managedPersistence: managed,
	}
	me.tradeBacklogLimit = int64(tradeBufferSize)
	if me.tradeBacklogLimit < 1 {
		me.tradeBacklogLimit = 1
	}
	go me.matchingLoop()
	return me
}

func (me *MatchingEngine) AddDerivativeInstrument(instrument DerivativeInstrument) error {
	var result error
	if !me.runCommand(func() { result = me.riskManager.addDerivativeInstrument(instrument) }) {
		return ErrEngineClosed
	}
	return result
}

func (me *MatchingEngine) SetDerivativeMarkPrice(symbol string, price uint64) error {
	var result error
	if !me.runCommand(func() {
		if me.priceFeeds[symbol] != nil {
			result = ErrPriceFeedOwnsMark
			return
		}
		result = me.riskManager.setDerivativeMarkPrice(symbol, price)
		if result == nil {
			me.markTimes[symbol] = time.Now()
			me.cancelUndermarginedOrders()
		}
	}) {
		return ErrEngineClosed
	}
	return result
}

func (me *MatchingEngine) SettleDerivativeInstrument(symbol string, price uint64) error {
	var result error
	if !me.runCommand(func() {
		if me.priceFeeds[symbol] != nil {
			result = ErrPriceFeedOwnsMark
			return
		}
		result = me.riskManager.settleDerivativeInstrument(symbol, price)
		if result == nil {
			me.markTimes[symbol] = time.Now()
			me.cancelUndermarginedOrders()
		}
	}) {
		return ErrEngineClosed
	}
	return result
}

func (me *MatchingEngine) SubmitOrder(order *Order) error {
	if order == nil {
		return fmt.Errorf("%w: order is nil", ErrInvalidOrder)
	}
	copiedOrder := *order
	if err := validateNewOrder(copiedOrder); err != nil {
		return err
	}
	select {
	case <-me.stop:
		return ErrEngineClosed
	default:
	}
	if me.pendingTradeCount.Load() >= me.tradeBacklogLimit {
		return ErrBackpressure
	}
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
	defer me.riskManager.releasePendingDerivativeOrders()
	for {
		select {
		case <-me.stop:
			return
		default:
		}
		var output chan *Trade
		var next *Trade
		if len(me.pendingTrades) > 0 && !me.managedPersistence {
			output = me.tradeChan
			next = me.pendingTrades[0]
		}
		select {
		case <-me.stop:
			return
		case command := <-me.commandChan:
			if !me.managedPersistence {
				me.expirePriceFeeds()
			}
			command()
			if !me.managedPersistence {
				me.attemptLiquidations()
			}
		case <-ticker.C:
			if me.managedPersistence {
				continue
			}
			me.expirePriceFeeds()
			me.checkAndTriggerStopOrders()
			me.attemptLiquidations()
		case output <- next:
			me.pendingTrades[0] = nil
			me.pendingTrades = me.pendingTrades[1:]
			me.pendingTradeCount.Add(-1)
		}
	}
}

// processOrder handles a single order
func (me *MatchingEngine) processOrder(order *Order) error {
	if !me.priceFeedReady(order.Symbol) || !me.accountPriceFeedsReady(order.ClientID) {
		order.Status = Rejected
		return ErrPriceUnavailable
	}
	_, processed := me.requestOrders[order.ID]
	if processed || me.hasActiveOrder(order.ID, order.Symbol) {
		order.Status = Rejected
		return ErrOrderIDConflict
	}
	// Perform risk checks before processing
	riskResult := me.riskManager.checkAndReserveOrder(order)
	if !riskResult.Approved {
		order.Status = Rejected
		return fmt.Errorf("%w: %s", ErrOrderRejected, riskResult.Reason)
	}

	defer me.cancelUndermarginedOrders()
	defer me.releaseCompletedOrderReservation(order)

	// Get or create order book for symbol
	book := me.getOrCreateBook(order.Symbol)

	// Handle stop orders
	if order.Type == StopMarket || order.Type == StopLimit {
		me.addStopOrder(order)
		return nil
	}

	// Market orders are matched immediately
	if order.Type == Market {
		me.matchMarketOrder(book, order)
	} else {
		me.matchLimitOrder(book, order)
	}
	if order.Status == Rejected {
		return ErrOrderRejected
	}
	return nil
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
	if order.Status == Rejected {
		return
	}
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
	if order.Status == Rejected {
		return
	}
	if order.Remaining > 0 {
		order.Status = Open
		if order.Remaining < order.Quantity {
			order.Status = PartialFilled
		}
		if !book.addOrder(order) {
			order.Status = Rejected
		}
	} else {
		order.Status = Filled
	}
}

func (me *MatchingEngine) matchBidLimit(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		level := book.getBestAsk()
		if level == nil || level.Price > order.Price {
			return
		}
		for _, resting := range append([]*Order(nil), level.Orders...) {
			quantity := min(order.Remaining, resting.Remaining)
			if quantity > 0 && !me.executeTrade(book, order, resting, level, quantity) {
				return
			}
			if order.Remaining == 0 {
				return
			}
		}
	}
}

func (me *MatchingEngine) matchAskLimit(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		level := book.getBestBid()
		if level == nil || level.Price < order.Price {
			return
		}
		for _, resting := range append([]*Order(nil), level.Orders...) {
			quantity := min(order.Remaining, resting.Remaining)
			if quantity > 0 && !me.executeTrade(book, order, resting, level, quantity) {
				return
			}
			if order.Remaining == 0 {
				return
			}
		}
	}
}

func (me *MatchingEngine) matchBidMarket(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		level := book.getBestAsk()
		if level == nil {
			return
		}
		for _, resting := range append([]*Order(nil), level.Orders...) {
			quantity := min(order.Remaining, resting.Remaining)
			if quantity > 0 && !me.executeTrade(book, order, resting, level, quantity) {
				return
			}
			if order.Remaining == 0 {
				return
			}
		}
	}
}

func (me *MatchingEngine) matchAskMarket(book *OrderBook, order *Order) {
	for order.Remaining > 0 {
		level := book.getBestBid()
		if level == nil {
			return
		}
		for _, resting := range append([]*Order(nil), level.Orders...) {
			quantity := min(order.Remaining, resting.Remaining)
			if quantity > 0 && !me.executeTrade(book, order, resting, level, quantity) {
				return
			}
			if order.Remaining == 0 {
				return
			}
		}
	}
}

func (me *MatchingEngine) executeTrade(book *OrderBook, incoming, resting *Order, level *PriceLevel, quantity uint64) bool {
	bid, ask := incoming, resting
	if incoming.Side == Ask {
		bid, ask = resting, incoming
	}
	trade := &Trade{
		ID: uuid.New(), Symbol: book.symbol, Price: level.Price, Quantity: quantity,
		BidOrder: bid.ID, AskOrder: ask.ID, BidClient: bid.ClientID, AskClient: ask.ClientID,
		Timestamp: time.Now().UnixNano(),
	}
	if err := me.riskManager.settleTrade(trade); err != nil {
		incoming.Status = Rejected
		return false
	}
	incoming.Remaining -= quantity
	resting.Remaining -= quantity
	level.TotalQty -= quantity
	if resting.Remaining == 0 {
		resting.Status = Filled
		book.removeOrder(resting.ID)
	} else {
		resting.Status = PartialFilled
	}
	me.riskManager.CheckCircuitBreakerTrigger(book.symbol, level.Price)
	me.publishTrade(trade)
	return true
}

func (me *MatchingEngine) releaseCompletedOrderReservation(order *Order) {
	if order.Status == Filled || order.Status == Rejected {
		me.riskManager.releaseDerivativeOrder(order.ID, order.Symbol)
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
				me.riskManager.releaseDerivativeOrder(orderID, symbol)
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
	me.cancelUndermarginedOrders()
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
	defer me.releaseCompletedOrderReservation(order)
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
			me.riskManager.releaseDerivativeOrder(orderID, symbol)
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

func (me *MatchingEngine) cancelUndermarginedOrders() {
	if !me.riskManager.derivativesEnabled {
		return
	}
	accounts := me.riskManager.accountsWithMarginShortfall()
	me.cancelAccountOrders(accounts)
}

func (me *MatchingEngine) cancelAccountOrders(accounts map[uuid.UUID]bool) {
	if len(accounts) == 0 {
		return
	}
	for symbol, book := range me.books {
		for id, order := range book.orders {
			if accounts[order.ClientID] {
				order.Status = Cancelled
				me.riskManager.releaseDerivativeOrder(id, symbol)
				book.removeOrder(id)
			}
		}
	}
	for symbol, orders := range me.stopOrders {
		remaining := make([]*Order, 0, len(orders))
		for _, order := range orders {
			if accounts[order.ClientID] {
				order.Status = Cancelled
				me.riskManager.releaseDerivativeOrder(order.ID, symbol)
			} else {
				remaining = append(remaining, order)
			}
		}
		me.stopOrders[symbol] = remaining
	}
}
