package matching

import (
	"encoding/json/v2"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type durableReservation struct {
	Symbol      string
	OrderID     uuid.UUID
	Reservation derivativeReservation
}
type durableRequest struct {
	Order   Order
	Receipt OrderReceipt
}
type durableLiquidation struct {
	ClientID uuid.UUID
	Symbol   string
	Status   LiquidationStatus
}
type DurableTrade struct {
	Sequence uint64 `json:"sequence"`
	Trade    Trade  `json:"trade"`
}

type durableLiquidationPolicy struct {
	MaxSlippageBps     uint64
	MaxMarkAgeNanos    int64
	RetryIntervalNanos int64
}

type durableState struct {
	Version           int
	Checkpoint        uint64
	Books             map[string][]Order
	Stops             map[string][]Order
	Clients           map[uuid.UUID]*Client
	Instruments       map[string]DerivativeInstrument
	Positions         map[uuid.UUID]map[string]DerivativePosition
	Lots              map[uuid.UUID]map[string][]DerivativeLot
	Marks             map[string]uint64
	Reservations      []durableReservation
	Limits            map[uuid.UUID]*RiskLimits
	Breakers          map[string]*CircuitBreaker
	DailyVolume       map[uuid.UUID]map[string]uint64
	RiskStats         RiskStats
	Requests          map[string]durableRequest
	RequestLimit      int
	RequestSequence   uint64
	OrdersProcessed   uint64
	TradesProcessed   uint64
	LiquidationPolicy durableLiquidationPolicy
	Liquidations      []durableLiquidation
	Outbox            []DurableTrade
	TradeSequence     uint64
	AcknowledgedTrade uint64
}

func (durable *DurableMatchingEngine) captureState() durableState {
	engine := durable.engine
	rm := engine.riskManager
	state := durableState{
		Version:         1,
		Checkpoint:      durable.checkpoint,
		Books:           make(map[string][]Order),
		Stops:           make(map[string][]Order),
		Clients:         rm.clients,
		Instruments:     rm.derivativeInstruments,
		Positions:       rm.derivativePositions,
		Lots:            rm.derivativeLots,
		Marks:           rm.derivativeMarks,
		Limits:          rm.riskLimits,
		Breakers:        rm.circuitBreakers,
		DailyVolume:     rm.dailyVolume,
		RiskStats:       rm.statistics,
		Requests:        make(map[string]durableRequest),
		RequestLimit:    engine.requestLimit,
		RequestSequence: engine.requestSequence,
		OrdersProcessed: engine.ordersProcessed.Load(),
		TradesProcessed: engine.tradesProcessed.Load(),
		LiquidationPolicy: durableLiquidationPolicy{
			engine.liquidationPolicy.MaxSlippageBps,
			int64(engine.liquidationPolicy.MaxMarkAge),
			int64(engine.liquidationPolicy.RetryInterval),
		},
		Outbox:            durable.outbox,
		TradeSequence:     durable.tradeSequence,
		AcknowledgedTrade: durable.acknowledgedTrade,
	}
	for symbol, book := range engine.books {
		levels := append([]*PriceLevel(nil), book.bids.PriceHeap...)
		levels = append(levels, book.asks...)
		sort.SliceStable(levels, func(i, j int) bool { return levels[i].Price < levels[j].Price })
		state.Books[symbol] = []Order{}
		for _, level := range levels {
			for _, order := range level.Orders {
				state.Books[symbol] = append(state.Books[symbol], *order)
			}
		}
	}
	for symbol, orders := range engine.stopOrders {
		state.Stops[symbol] = []Order{}
		for _, order := range orders {
			state.Stops[symbol] = append(state.Stops[symbol], *order)
		}
	}
	for key, reservation := range rm.derivativeReservations {
		state.Reservations = append(state.Reservations, durableReservation{key.Symbol, key.OrderID, reservation})
	}
	sort.Slice(state.Reservations, func(i, j int) bool {
		a, b := state.Reservations[i], state.Reservations[j]
		if a.Symbol == b.Symbol {
			return a.OrderID.String() < b.OrderID.String()
		}
		return a.Symbol < b.Symbol
	})
	for key, request := range engine.requests {
		state.Requests[key] = durableRequest{request.order, request.receipt}
	}
	for key, status := range engine.liquidations {
		state.Liquidations = append(state.Liquidations, durableLiquidation{key.ClientID, key.Symbol, status})
	}
	sort.Slice(state.Liquidations, func(i, j int) bool {
		a, b := state.Liquidations[i], state.Liquidations[j]
		if a.ClientID == b.ClientID {
			return a.Symbol < b.Symbol
		}
		return a.ClientID.String() < b.ClientID.String()
	})
	return state
}

func (durable *DurableMatchingEngine) encodeState() ([]byte, error) {
	return json.Marshal(durable.captureState(), json.Deterministic(true))
}

func (durable *DurableMatchingEngine) restoreState(state durableState) error {
	if state.Version != 1 || state.Checkpoint == 0 || state.Clients == nil || state.Instruments == nil || state.Positions == nil || state.Lots == nil || state.Marks == nil || state.Limits == nil || state.Breakers == nil || state.DailyVolume == nil || state.Books == nil || state.Stops == nil || state.Requests == nil || state.RequestLimit < 1 || state.RequestLimit < len(state.Requests) || state.RequestSequence != uint64(len(state.Requests)) || state.OrdersProcessed != state.RequestSequence || state.TradesProcessed != state.TradeSequence || state.AcknowledgedTrade > state.TradeSequence || uint64(len(state.Outbox)) != state.TradeSequence-state.AcknowledgedTrade {
		return ErrInvalidDurableState
	}
	engine := durable.engine
	rm := engine.riskManager
	rm.clients = state.Clients
	rm.derivativeInstruments = state.Instruments
	rm.derivativePositions = state.Positions
	rm.derivativeLots = state.Lots
	rm.derivativeMarks = state.Marks
	rm.riskLimits = state.Limits
	rm.circuitBreakers = state.Breakers
	rm.dailyVolume = state.DailyVolume
	rm.statistics = state.RiskStats
	for symbol, instrument := range state.Instruments {
		if symbol != instrument.Symbol || rm.addDerivativeInstrument(instrument) != nil {
			return ErrInvalidDurableState
		}
	}
	for id, client := range state.Clients {
		if client == nil || id == uuid.Nil || client.ID != id || client.Collateral > math.MaxInt64 || client.MarginCurrency == "" || strings.TrimSpace(client.MarginCurrency) != client.MarginCurrency {
			return ErrInvalidDurableState
		}
	}
	for symbol, mark := range state.Marks {
		if _, known := state.Instruments[symbol]; !known || mark == 0 {
			return ErrInvalidDurableState
		}
	}
	for id, limits := range state.Limits {
		if id == uuid.Nil || limits == nil {
			return ErrInvalidDurableState
		}
	}
	for symbol, breaker := range state.Breakers {
		if breaker == nil || symbol != breaker.Symbol || rm.AddCircuitBreaker(breaker) != nil {
			return ErrInvalidDurableState
		}
	}
	engine.requestLimit = state.RequestLimit
	engine.requestSequence = state.RequestSequence
	sequences := make(map[uint64]bool)
	for key, request := range state.Requests {
		receipt := request.Receipt
		if !ValidRequestKey(key) || ValidateNewOrder(request.Order) != nil || receipt.OrderID != request.Order.ID || receipt.Symbol != request.Order.Symbol || receipt.Sequence == 0 || receipt.Sequence > state.RequestSequence || sequences[receipt.Sequence] || receipt.RemainingQuantity > request.Order.Quantity || receipt.FilledQuantity != request.Order.Quantity-receipt.RemainingQuantity {
			return ErrInvalidDurableState
		}
		if _, exists := engine.requestOrders[request.Order.ID]; exists {
			return ErrInvalidDurableState
		}
		sequences[receipt.Sequence] = true
		engine.requests[key] = processedOrderRequest{request.Order, receipt}
		engine.requestOrders[request.Order.ID] = key
	}
	active := make(map[uuid.UUID]Order)
	for symbol, orders := range state.Books {
		book := NewOrderBook(symbol)
		for _, value := range orders {
			order := value
			if order.Symbol != symbol || order.Type != Limit || (order.Status != Open && order.Status != PartialFilled) || !durable.validRestoredOrder(order, active) || !book.addOrder(&order) {
				return ErrInvalidDurableState
			}
			active[order.ID] = order
		}
		book.updateSnapshot()
		engine.books[symbol] = book
	}
	for symbol, orders := range state.Stops {
		for _, value := range orders {
			order := value
			if order.Symbol != symbol || (order.Type != StopMarket && order.Type != StopLimit) || order.Status != Pending || order.StopPrice == 0 || !durable.validRestoredOrder(order, active) {
				return ErrInvalidDurableState
			}
			active[order.ID] = order
			engine.stopOrders[symbol] = append(engine.stopOrders[symbol], &order)
		}
	}
	if len(state.Reservations) != len(active) {
		return ErrInvalidDurableState
	}
	pending := make(map[uuid.UUID]map[string]DerivativePosition)
	for _, entry := range state.Reservations {
		key := derivativeOrderKey{entry.Symbol, entry.OrderID}
		reservation := entry.Reservation
		order, exists := active[entry.OrderID]
		if _, duplicate := rm.derivativeReservations[key]; duplicate || !exists || order.Symbol != entry.Symbol || reservation.ClientID != order.ClientID || reservation.Side != order.Side || reservation.Remaining != order.Remaining {
			return ErrInvalidDurableState
		}
		rm.derivativeReservations[key] = reservation
		if pending[order.ClientID] == nil {
			pending[order.ClientID] = make(map[string]DerivativePosition)
		}
		position := pending[order.ClientID][order.Symbol]
		if order.Side == Bid {
			if position.OpenBuyContracts > math.MaxInt64-order.Remaining {
				return ErrInvalidDurableState
			}
			position.OpenBuyContracts += order.Remaining
		} else {
			if position.OpenSellContracts > math.MaxInt64-order.Remaining {
				return ErrInvalidDurableState
			}
			position.OpenSellContracts += order.Remaining
		}
		pending[order.ClientID][order.Symbol] = position
	}
	nets := make(map[string]*big.Int)
	for id, positions := range state.Positions {
		if state.Clients[id] == nil {
			return ErrInvalidDurableState
		}
		for symbol, position := range positions {
			if _, known := state.Instruments[symbol]; !known || position.NetContracts == math.MinInt64 || position.OpenBuyContracts != pending[id][symbol].OpenBuyContracts || position.OpenSellContracts != pending[id][symbol].OpenSellContracts {
				return ErrInvalidDurableState
			}
			lotsTotal := uint64(0)
			for _, lot := range state.Lots[id][symbol] {
				if lot.Quantity == 0 || lot.Quantity > math.MaxInt64 || lot.EntryPrice == 0 || lotsTotal > math.MaxInt64-lot.Quantity {
					return ErrInvalidDurableState
				}
				lotsTotal += lot.Quantity
			}
			if lotsTotal != absoluteContracts(position.NetContracts) {
				return ErrInvalidDurableState
			}
			if nets[symbol] == nil {
				nets[symbol] = new(big.Int)
			}
			nets[symbol].Add(nets[symbol], big.NewInt(position.NetContracts))
		}
	}
	for id, positions := range pending {
		for symbol, position := range positions {
			stored := state.Positions[id][symbol]
			if stored.OpenBuyContracts != position.OpenBuyContracts || stored.OpenSellContracts != position.OpenSellContracts {
				return ErrInvalidDurableState
			}
		}
	}
	for id, symbols := range state.Lots {
		for symbol, lots := range symbols {
			if _, exists := state.Positions[id][symbol]; !exists && len(lots) > 0 {
				return ErrInvalidDurableState
			}
		}
	}
	for _, net := range nets {
		if net.Sign() != 0 {
			return ErrInvalidDurableState
		}
	}
	for id := range state.Clients {
		if _, err := rm.calculateMarginAccountLocked(id, "", nil, nil); err != nil {
			return ErrInvalidDurableState
		}
	}
	for index, event := range state.Outbox {
		trade := event.Trade
		if _, known := state.Instruments[trade.Symbol]; !known {
			return ErrInvalidDurableState
		}
		if event.Sequence != state.AcknowledgedTrade+uint64(index)+1 || trade.ID == uuid.Nil || trade.Quantity == 0 || trade.Price == 0 || state.Clients[trade.BidClient] == nil || state.Clients[trade.AskClient] == nil || trade.BidClient == trade.AskClient {
			return ErrInvalidDurableState
		}
	}
	policy := LiquidationPolicy{state.LiquidationPolicy.MaxSlippageBps, time.Duration(state.LiquidationPolicy.MaxMarkAgeNanos), time.Duration(state.LiquidationPolicy.RetryIntervalNanos)}
	if policy != (LiquidationPolicy{}) && (policy.MaxSlippageBps > 10000 || policy.MaxMarkAge <= 0 || policy.RetryInterval <= 0) {
		return ErrInvalidDurableState
	}
	engine.liquidationPolicy = policy
	for _, entry := range state.Liquidations {
		engine.liquidations[liquidationKey{entry.ClientID, entry.Symbol}] = entry.Status
	}
	engine.ordersProcessed.Store(state.OrdersProcessed)
	engine.tradesProcessed.Store(state.TradesProcessed)
	durable.checkpoint = state.Checkpoint
	durable.outbox = state.Outbox
	durable.tradeSequence = state.TradeSequence
	durable.acknowledgedTrade = state.AcknowledgedTrade
	return nil
}

func (durable *DurableMatchingEngine) validRestoredOrder(order Order, active map[uuid.UUID]Order) bool {
	if _, exists := active[order.ID]; exists {
		return false
	}
	requestKey, exists := durable.engine.requestOrders[order.ID]
	if !exists {
		return false
	}
	original := durable.engine.requests[requestKey].order
	if original.ClientID != order.ClientID || original.Symbol != order.Symbol || original.Side != order.Side || original.Quantity != order.Quantity || order.Remaining == 0 || order.Remaining > order.Quantity || order.Remaining > math.MaxInt64 || (order.Side != Bid && order.Side != Ask) || durable.engine.riskManager.clients[order.ClientID] == nil {
		return false
	}
	_, known := durable.engine.riskManager.derivativeInstruments[order.Symbol]
	return known && (order.Type == StopMarket || order.Price > 0)
}
