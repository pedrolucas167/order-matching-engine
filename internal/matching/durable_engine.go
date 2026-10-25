package matching

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

type DurableMatchingEngine struct {
	mu                sync.Mutex
	engine            *MatchingEngine
	db                *bolt.DB
	writeState        func([]byte) error
	checkpoint        uint64
	tradeSequence     uint64
	acknowledgedTrade uint64
	outbox            []DurableTrade
	outboxLimit       int
	lastState         []byte
	failure           error
	closed            bool
	stop              chan struct{}
	done              chan struct{}
	admission         chan struct{}
}

type TradeDeliveryState struct {
	Acknowledged uint64
	Latest       uint64
	Pending      int
}

func (durable *DurableMatchingEngine) GetTradeDeliveryState() (TradeDeliveryState, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return TradeDeliveryState{}, err
	}
	return TradeDeliveryState{durable.acknowledgedTrade, durable.tradeSequence, len(durable.outbox)}, nil
}

func (durable *DurableMatchingEngine) ResetDailyVolume(id uuid.UUID) error {
	return durable.mutate(func() error { durable.engine.riskManager.ResetDailyVolume(id); return nil })
}

func NewDurableDerivativesMatchingEngine(path string, orderBufferSize, tradeBufferSize int) (*DurableMatchingEngine, error) {
	if orderBufferSize < 1 || tradeBufferSize < 1 {
		return nil, ErrInvalidDurableState
	}
	db, state, err := openDurableStore(path)
	if err != nil {
		return nil, err
	}
	durable := &DurableMatchingEngine{engine: newEngineMode(orderBufferSize, tradeBufferSize, true, true), db: db, checkpoint: 1, outboxLimit: tradeBufferSize, stop: make(chan struct{}), done: make(chan struct{}), admission: make(chan struct{}, orderBufferSize)}
	durable.writeState = func(payload []byte) error { return writeDurableState(db, payload) }
	var initializeError error
	durable.engine.runCommand(func() {
		if state != nil {
			initializeError = durable.restoreState(*state)
			if initializeError != nil {
				return
			}
		}
		durable.lastState, initializeError = durable.encodeState()
		if initializeError == nil && state == nil {
			initializeError = durable.writeState(durable.lastState)
		}
	})
	if initializeError != nil {
		durable.engine.Close()
		db.Close()
		return nil, initializeError
	}
	go durable.maintenanceLoop()
	return durable, nil
}

func (durable *DurableMatchingEngine) healthError() error {
	if durable.failure != nil {
		return durable.failure
	}
	if durable.closed {
		return ErrEngineClosed
	}
	return nil
}

func (durable *DurableMatchingEngine) commit(action func() error) error {
	if err := durable.healthError(); err != nil {
		return err
	}
	var actionError, commitError error
	if !durable.engine.runCommand(func() {
		actionError = action()
		if len(durable.outbox) < durable.outboxLimit {
			durable.engine.attemptLiquidations()
		}
		for _, trade := range durable.engine.pendingTrades {
			if durable.tradeSequence == math.MaxUint64 {
				commitError = ErrInvalidDurableState
				return
			}
			durable.tradeSequence++
			durable.outbox = append(durable.outbox, DurableTrade{durable.tradeSequence, *trade})
		}
		durable.engine.pendingTrades = nil
		durable.engine.pendingTradeCount.Store(0)
		payload, err := durable.encodeState()
		if err != nil {
			commitError = err
			return
		}
		if bytes.Equal(payload, durable.lastState) {
			return
		}
		if durable.checkpoint == math.MaxUint64 {
			commitError = ErrInvalidDurableState
			return
		}
		durable.checkpoint++
		payload, err = durable.encodeState()
		if err != nil {
			commitError = err
			return
		}
		if err = durable.writeState(payload); err != nil {
			commitError = err
			return
		}
		durable.lastState = payload
	}) {
		return ErrEngineClosed
	}
	if commitError != nil {
		durable.failure = fmt.Errorf("%w: %w", ErrDurableStorage, commitError)
		durable.engine.Close()
		return durable.failure
	}
	return actionError
}

func (durable *DurableMatchingEngine) SubmitOrderRequest(ctx context.Context, key string, order *Order) (OrderReceipt, bool, error) {
	if ctx == nil || !ValidRequestKey(key) {
		return OrderReceipt{}, false, ErrInvalidOrderRequest
	}
	if order == nil {
		return OrderReceipt{}, false, ErrInvalidOrder
	}
	copied := *order
	if err := ValidateNewOrder(copied); err != nil {
		return OrderReceipt{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return OrderReceipt{}, false, err
	}
	select {
	case durable.admission <- struct{}{}:
		defer func() { <-durable.admission }()
	case <-ctx.Done():
		return OrderReceipt{}, false, ctx.Err()
	default:
		return OrderReceipt{}, false, ErrBackpressure
	}
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return OrderReceipt{}, false, err
	}
	var result orderRequestResult
	err := durable.commit(func() error {
		if _, replay := durable.engine.requests[key]; !replay && len(durable.outbox) >= durable.outboxLimit {
			return ErrBackpressure
		}
		result = durable.engine.processOrderRequest(key, copied)
		return result.err
	})
	if err != nil {
		if errors.Is(err, ErrDurableStorage) {
			return OrderReceipt{}, false, fmt.Errorf("%w: %w", ErrRequestOutcomeUnknown, err)
		}
		return OrderReceipt{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return OrderReceipt{}, false, fmt.Errorf("%w: %w", ErrRequestOutcomeUnknown, err)
	}
	return result.receipt, result.replayed, nil
}

func (durable *DurableMatchingEngine) mutate(action func() error) error {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	return durable.commit(action)
}

func (durable *DurableMatchingEngine) AddClient(client *Client) error {
	if client == nil {
		return ErrClientNotFound
	}
	copied := *client
	return durable.mutate(func() error { return durable.engine.riskManager.AddClient(&copied) })
}

func (durable *DurableMatchingEngine) AddDerivativeInstrument(instrument DerivativeInstrument) error {
	return durable.mutate(func() error { return durable.engine.riskManager.addDerivativeInstrument(instrument) })
}

func (durable *DurableMatchingEngine) SetRiskLimits(id uuid.UUID, limits *RiskLimits) error {
	if limits == nil {
		return ErrInvalidRiskLimits
	}
	copied := *limits
	return durable.mutate(func() error { return durable.engine.riskManager.SetRiskLimits(id, &copied) })
}

func (durable *DurableMatchingEngine) AddCircuitBreaker(breaker *CircuitBreaker) error {
	if breaker == nil {
		return ErrInvalidCircuitBreaker
	}
	copied := *breaker
	return durable.mutate(func() error { return durable.engine.riskManager.AddCircuitBreaker(&copied) })
}

func (durable *DurableMatchingEngine) SetDerivativeMarkPrice(symbol string, price uint64) error {
	return durable.mutate(func() error {
		if err := durable.engine.riskManager.setDerivativeMarkPrice(symbol, price); err != nil {
			return err
		}
		durable.engine.markTimes[symbol] = time.Now()
		durable.engine.cancelUndermarginedOrders()
		return nil
	})
}

func (durable *DurableMatchingEngine) SettleDerivativeInstrument(symbol string, price uint64) error {
	return durable.mutate(func() error {
		if err := durable.engine.riskManager.settleDerivativeInstrument(symbol, price); err != nil {
			return err
		}
		durable.engine.markTimes[symbol] = time.Now()
		durable.engine.cancelUndermarginedOrders()
		return nil
	})
}

func (durable *DurableMatchingEngine) CancelOrder(id uuid.UUID, symbol string) (bool, error) {
	var canceled bool
	err := durable.mutate(func() error {
		if book := durable.engine.books[symbol]; book != nil {
			if order, exists := book.orders[id]; exists {
				order.Status = Cancelled
				durable.engine.riskManager.releaseDerivativeOrder(id, symbol)
				book.removeOrder(id)
				canceled = true
			}
		}
		return nil
	})
	return canceled && err == nil, err
}

func (durable *DurableMatchingEngine) CancelStopOrder(id uuid.UUID, symbol string) (bool, error) {
	var canceled bool
	err := durable.mutate(func() error { canceled = durable.engine.cancelStopOrder(id, symbol); return nil })
	return canceled && err == nil, err
}

func (durable *DurableMatchingEngine) SetOrderRequestCapacity(limit int) error {
	if limit < 1 {
		return ErrRequestCapacity
	}
	return durable.mutate(func() error {
		if limit < len(durable.engine.requests) {
			return ErrRequestCapacity
		}
		durable.engine.requestLimit = limit
		return nil
	})
}

func (durable *DurableMatchingEngine) SetLiquidationPolicy(policy LiquidationPolicy) error {
	if policy != (LiquidationPolicy{}) && (policy.MaxSlippageBps > 10000 || policy.MaxMarkAge <= 0 || policy.RetryInterval <= 0) {
		return errors.New("invalid liquidation policy")
	}
	return durable.mutate(func() error { durable.engine.liquidationPolicy = policy; return nil })
}

func (durable *DurableMatchingEngine) ReadTrades(after uint64, limit int) ([]DurableTrade, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 10000 {
		return nil, errors.New("trade page limit must be between 1 and 10000")
	}
	if after < durable.acknowledgedTrade || after > durable.tradeSequence {
		return nil, errors.New("trade cursor is outside the retained range")
	}
	result := make([]DurableTrade, 0, min(uint64(limit), uint64(len(durable.outbox))))
	for _, event := range durable.outbox {
		if event.Sequence > after {
			result = append(result, event)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (durable *DurableMatchingEngine) AcknowledgeTrades(sequence uint64) error {
	return durable.mutate(func() error {
		if sequence > durable.tradeSequence {
			return errors.New("cannot acknowledge an unknown trade")
		}
		if sequence <= durable.acknowledgedTrade {
			return nil
		}
		remaining := durable.outbox[:0]
		for _, event := range durable.outbox {
			if event.Sequence > sequence {
				remaining = append(remaining, event)
			}
		}
		durable.outbox = remaining
		durable.acknowledgedTrade = sequence
		return nil
	})
}

func (durable *DurableMatchingEngine) GetMarginAccount(id uuid.UUID) (MarginAccount, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return MarginAccount{}, err
	}
	account, exists := durable.engine.riskManager.GetMarginAccount(id)
	if !exists {
		return MarginAccount{}, ErrClientNotFound
	}
	return account, nil
}

func (durable *DurableMatchingEngine) GetClient(id uuid.UUID) (Client, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return Client{}, err
	}
	client, exists := durable.engine.riskManager.GetClient(id)
	if !exists {
		return Client{}, ErrClientNotFound
	}
	return *client, nil
}

func (durable *DurableMatchingEngine) GetDerivativePosition(id uuid.UUID, symbol string) (DerivativePosition, bool, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return DerivativePosition{}, false, err
	}
	position, exists := durable.engine.riskManager.GetDerivativePosition(id, symbol)
	return position, exists, nil
}

func (durable *DurableMatchingEngine) GetSnapshot(symbol string) (*OrderBookSnapshot, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return nil, err
	}
	return durable.engine.GetSnapshot(symbol), nil
}

func (durable *DurableMatchingEngine) GetStats() (EngineStats, error) {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if err := durable.healthError(); err != nil {
		return EngineStats{}, err
	}
	return durable.engine.GetStats(), nil
}

func (durable *DurableMatchingEngine) maintenanceLoop() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	defer close(durable.done)
	for {
		select {
		case <-durable.stop:
			return
		case <-ticker.C:
			durable.mu.Lock()
			needsMaintenance := durable.engine.countStopOrders() > 0 || durable.engine.liquidationPolicy.MaxMarkAge > 0
			for _, breaker := range durable.engine.riskManager.circuitBreakers {
				if breaker.State == CircuitBreakerOpen {
					needsMaintenance = true
					break
				}
			}
			if durable.healthError() == nil && needsMaintenance {
				_ = durable.commit(func() error {
					durable.engine.riskManager.checkCooldowns()
					if len(durable.outbox) < durable.outboxLimit {
						durable.engine.checkAndTriggerStopOrders()
					}
					return nil
				})
			}
			durable.mu.Unlock()
		}
	}
}

func (durable *DurableMatchingEngine) Close() error {
	durable.mu.Lock()
	if durable.closed {
		durable.mu.Unlock()
		<-durable.done
		return nil
	}
	durable.closed = true
	close(durable.stop)
	durable.engine.Close()
	err := durable.db.Close()
	durable.mu.Unlock()
	<-durable.done
	return err
}
