package main

import (
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
)

type LiquidationPolicy struct {
	MaxSlippageBps uint64
	MaxMarkAge     time.Duration
	RetryInterval  time.Duration
}

type LiquidationStatus struct {
	AttemptedQuantity uint64
	FilledQuantity    uint64
	RemainingQuantity uint64
	LimitPrice        uint64
	LastAttempt       time.Time
	Reason            string
}

type liquidationKey struct {
	ClientID uuid.UUID
	Symbol   string
}

func (me *MatchingEngine) SetLiquidationPolicy(policy LiquidationPolicy) error {
	if policy != (LiquidationPolicy{}) && (policy.MaxSlippageBps > 10000 || policy.MaxMarkAge <= 0 || policy.RetryInterval <= 0) {
		return errors.New("invalid liquidation policy")
	}
	var result error
	if !me.runCommand(func() {
		if !me.riskManager.derivativesEnabled {
			result = errors.New("liquidation requires a derivatives engine")
			return
		}
		me.liquidationPolicy = policy
	}) {
		return ErrEngineClosed
	}
	return result
}

func (me *MatchingEngine) GetLiquidationStatus(clientID uuid.UUID, symbol string) (LiquidationStatus, bool) {
	var status LiquidationStatus
	var exists bool
	me.runCommand(func() { status, exists = me.liquidations[liquidationKey{clientID, symbol}] })
	return status, exists
}

func liquidationLimit(mark, bps uint64, side Side) (uint64, error) {
	factor := uint64(10000) + bps
	if side == Ask {
		factor = 10000 - bps
	}
	value := new(big.Int).Mul(new(big.Int).SetUint64(mark), new(big.Int).SetUint64(factor))
	if side == Ask {
		value.Add(value, big.NewInt(9999))
	}
	value.Quo(value, big.NewInt(10000))
	if !value.IsUint64() {
		return 0, ErrRiskArithmeticOverflow
	}
	return max(uint64(1), value.Uint64()), nil
}

func (me *MatchingEngine) liquidationCandidates() []liquidationKey {
	rm := me.riskManager
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	var keys []liquidationKey
	for clientID, positions := range rm.derivativePositions {
		account, err := rm.calculateMarginAccountLocked(clientID, "", nil, nil)
		if err != nil || account.MaintenanceMarginShortfall == 0 {
			continue
		}
		for symbol, position := range positions {
			if position.NetContracts != 0 {
				keys = append(keys, liquidationKey{clientID, symbol})
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ClientID == keys[j].ClientID {
			return keys[i].Symbol < keys[j].Symbol
		}
		return keys[i].ClientID.String() < keys[j].ClientID.String()
	})
	return keys
}

func (me *MatchingEngine) attemptLiquidations() {
	policy := me.liquidationPolicy
	if policy.MaxMarkAge == 0 {
		return
	}
	me.cancelUndermarginedOrders()
	for _, key := range me.liquidationCandidates() {
		account, _ := me.riskManager.GetMarginAccount(key.ClientID)
		if account.MaintenanceMarginShortfall == 0 {
			continue
		}
		previous := me.liquidations[key]
		if time.Since(previous.LastAttempt) < policy.RetryInterval {
			continue
		}
		position, _ := me.riskManager.GetDerivativePosition(key.ClientID, key.Symbol)
		if position.NetContracts == 0 {
			continue
		}
		quantity := uint64(position.NetContracts)
		side := Ask
		if position.NetContracts < 0 {
			quantity = uint64(-position.NetContracts)
			side = Bid
		}
		status := LiquidationStatus{AttemptedQuantity: quantity, RemainingQuantity: quantity, LastAttempt: time.Now()}
		mark, _ := me.riskManager.GetDerivativeMarkPrice(key.Symbol)
		stamp := me.markTimes[key.Symbol]
		if stamp.IsZero() || !me.accountMarksFresh(key.ClientID, policy.MaxMarkAge) {
			status.Reason = "stale price"
			me.liquidations[key] = status
			continue
		}
		if me.pendingTradeCount.Load() >= me.tradeBacklogLimit {
			status.Reason = "trade publication backlog"
			me.liquidations[key] = status
			continue
		}
		limit, err := liquidationLimit(mark, policy.MaxSlippageBps, side)
		status.LimitPrice = limit
		if err != nil {
			status.Reason = "price overflow"
			me.liquidations[key] = status
			continue
		}
		order := &Order{ID: uuid.New(), ClientID: key.ClientID, Symbol: key.Symbol, Side: side, Type: Market, Quantity: quantity, Remaining: quantity, Status: Pending, Price: limit}
		risk := me.riskManager.checkAndReserveOrder(order)
		if !risk.Approved {
			status.Reason = "risk control"
			me.liquidations[key] = status
			continue
		}
		book := me.getOrCreateBook(key.Symbol)
		if side == Bid {
			me.matchBidLimit(book, order)
		} else {
			me.matchAskLimit(book, order)
		}
		me.riskManager.releaseDerivativeOrder(order.ID, order.Symbol)
		status.FilledQuantity = quantity - order.Remaining
		status.RemainingQuantity = order.Remaining
		status.Reason = "insufficient liquidity within price limit"
		if order.Status == Rejected {
			status.Reason = "settlement rejected"
		} else if order.Remaining == 0 {
			status.Reason = "completed"
		}
		me.liquidations[key] = status
		me.cancelUndermarginedOrders()
	}
}

func (me *MatchingEngine) accountMarksFresh(clientID uuid.UUID, maxAge time.Duration) bool {
	me.riskManager.accountMu.RLock()
	defer me.riskManager.accountMu.RUnlock()
	for symbol, position := range me.riskManager.derivativePositions[clientID] {
		if position.NetContracts == 0 {
			continue
		}
		stamp := me.markTimes[symbol]
		if stamp.IsZero() || time.Since(stamp) > maxAge || !me.priceFeedReady(symbol) {
			return false
		}
	}
	return true
}
