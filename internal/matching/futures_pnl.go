package matching

import (
	"math"
	"math/big"

	"github.com/google/uuid"
)

type pnlTotals struct {
	realized   big.Int
	unrealized big.Int
}

func signedMoney(value *big.Int) (int64, error) {
	if !value.IsInt64() || value.Int64() == math.MinInt64 {
		return 0, ErrRiskArithmeticOverflow
	}
	return value.Int64(), nil
}

type DerivativeLot struct {
	Quantity   uint64
	EntryPrice uint64
}

func addMoney(first, second int64) (int64, error) {
	if first == math.MinInt64 || second == math.MinInt64 || (second > 0 && first > math.MaxInt64-second) || (second < 0 && first < -math.MaxInt64-second) {
		return 0, ErrRiskArithmeticOverflow
	}
	return first + second, nil
}

func contractPnL(entry, current, quantity, multiplier uint64, long bool) (int64, error) {
	difference := uint64(0)
	positive := current >= entry
	if positive {
		difference = current - entry
	} else {
		difference = entry - current
	}
	if !long {
		positive = !positive
	}
	if quantity == 0 || difference == 0 {
		return 0, nil
	}
	if multiplier == 0 || difference > math.MaxInt64/quantity {
		return 0, ErrRiskArithmeticOverflow
	}
	amount := difference * quantity
	if amount > math.MaxInt64/multiplier {
		return 0, ErrRiskArithmeticOverflow
	}
	result := int64(amount * multiplier)
	if !positive {
		result = -result
	}
	return result, nil
}

func applyDerivativeFill(position DerivativePosition, lots []DerivativeLot, side Side, quantity, price, multiplier uint64) (DerivativePosition, []DerivativeLot, error) {
	updated := position
	net, err := changeNetContracts(position.NetContracts, quantity, side)
	if err != nil {
		return DerivativePosition{}, nil, err
	}
	updated.NetContracts = net
	updated.UnrealizedPnL = 0
	remainingLots := append([]DerivativeLot(nil), lots...)
	remaining := quantity
	realized := big.NewInt(position.RealizedPnL)
	closing := (position.NetContracts > 0 && side == Ask) || (position.NetContracts < 0 && side == Bid)
	if closing {
		toClose := min(quantity, absoluteContracts(position.NetContracts))
		remaining -= toClose
		for toClose > 0 {
			if len(remainingLots) == 0 {
				return DerivativePosition{}, nil, ErrInvalidDerivativeTrade
			}
			closed := min(toClose, remainingLots[0].Quantity)
			pnl, err := contractPnL(remainingLots[0].EntryPrice, price, closed, multiplier, position.NetContracts > 0)
			if err != nil {
				return DerivativePosition{}, nil, err
			}
			realized.Add(realized, big.NewInt(pnl))
			toClose -= closed
			remainingLots[0].Quantity -= closed
			if remainingLots[0].Quantity == 0 {
				remainingLots = remainingLots[1:]
			}
		}
	}
	if remaining > 0 {
		if len(remainingLots) > 0 && remainingLots[len(remainingLots)-1].EntryPrice == price {
			remainingLots[len(remainingLots)-1].Quantity += remaining
		} else {
			remainingLots = append(remainingLots, DerivativeLot{Quantity: remaining, EntryPrice: price})
		}
	}
	updated.RealizedPnL, err = signedMoney(realized)
	if err != nil {
		return DerivativePosition{}, nil, err
	}
	return updated, remainingLots, nil
}

func positionPnL(position DerivativePosition, lots []DerivativeLot, mark, multiplier uint64) (int64, error) {
	if position.NetContracts == 0 {
		return 0, nil
	}
	if mark == 0 {
		return 0, ErrUnknownMarkPrice
	}
	result := new(big.Int)
	for _, lot := range lots {
		pnl, err := contractPnL(lot.EntryPrice, mark, lot.Quantity, multiplier, position.NetContracts > 0)
		if err != nil {
			return 0, err
		}
		result.Add(result, big.NewInt(pnl))
	}
	return signedMoney(result)
}

func marginShortfall(required uint64, equity int64) uint64 {
	if equity < 0 {
		return required + uint64(-equity)
	}
	if uint64(equity) < required {
		return required - uint64(equity)
	}
	return 0
}

func (rm *RiskManager) addValuedPositionLocked(account *MarginAccount, symbol string, position DerivativePosition, lots []DerivativeLot, totals *pnlTotals) error {
	instrument := rm.derivativeInstruments[symbol]
	requirement, err := calculatePositionMargin(position, instrument.InitialMarginPerContract)
	if err != nil {
		return err
	}
	if err := addMarginRequirement(account, requirement); err != nil {
		return err
	}
	contracts := absoluteContracts(position.NetContracts)
	if contracts > 0 && instrument.MaintenanceMarginPerContract > math.MaxInt64/contracts {
		return ErrRiskArithmeticOverflow
	}
	maintenance := contracts * instrument.MaintenanceMarginPerContract
	if account.MaintenanceMargin > math.MaxInt64-maintenance {
		return ErrRiskArithmeticOverflow
	}
	account.MaintenanceMargin += maintenance
	unrealized, err := positionPnL(position, lots, rm.derivativeMarks[symbol], instrument.ContractMultiplier)
	if err != nil {
		return err
	}
	totals.unrealized.Add(&totals.unrealized, big.NewInt(unrealized))
	totals.realized.Add(&totals.realized, big.NewInt(position.RealizedPnL))
	return nil
}

func isClosingMarketOrder(order *Order, position DerivativePosition) bool {
	if order.Type != Market || position.OpenBuyContracts != 0 || position.OpenSellContracts != 0 || order.Remaining > absoluteContracts(position.NetContracts) {
		return false
	}
	return (position.NetContracts > 0 && order.Side == Ask) || (position.NetContracts < 0 && order.Side == Bid)
}

func (rm *RiskManager) storeDerivativeLotsLocked(clientID uuid.UUID, symbol string, lots []DerivativeLot) {
	if rm.derivativeLots[clientID] == nil {
		rm.derivativeLots[clientID] = make(map[string][]DerivativeLot)
	}
	if len(lots) == 0 {
		delete(rm.derivativeLots[clientID], symbol)
	} else {
		rm.derivativeLots[clientID][symbol] = lots
	}
}

func (rm *RiskManager) GetDerivativeLots(clientID uuid.UUID, symbol string) ([]DerivativeLot, bool) {
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	_, exists := rm.derivativePositions[clientID][symbol]
	return append([]DerivativeLot(nil), rm.derivativeLots[clientID][symbol]...), exists
}

func (rm *RiskManager) GetDerivativeMarkPrice(symbol string) (uint64, bool) {
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	mark, exists := rm.derivativeMarks[symbol]
	return mark, exists
}

func (rm *RiskManager) setDerivativeMarkPrice(symbol string, price uint64) error {
	if price == 0 {
		return ErrUnknownMarkPrice
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	if _, exists := rm.derivativeInstruments[symbol]; !exists {
		return ErrUnknownDerivativeInstrument
	}
	previous, hadMark := rm.derivativeMarks[symbol]
	rm.derivativeMarks[symbol] = price
	for clientID := range rm.clients {
		if _, err := rm.calculateMarginAccountLocked(clientID, "", nil, nil); err != nil {
			if hadMark {
				rm.derivativeMarks[symbol] = previous
			} else {
				delete(rm.derivativeMarks, symbol)
			}
			return err
		}
	}
	return nil
}

type settledDerivativePosition struct {
	clientID uuid.UUID
	position DerivativePosition
	lots     []DerivativeLot
}

func (rm *RiskManager) settleDerivativeInstrument(symbol string, price uint64) error {
	if price == 0 {
		return ErrUnknownMarkPrice
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	instrument, exists := rm.derivativeInstruments[symbol]
	if !exists {
		return ErrUnknownDerivativeInstrument
	}
	previous, hadMark := rm.derivativeMarks[symbol]
	rm.derivativeMarks[symbol] = price
	committed := false
	defer func() {
		if !committed {
			if hadMark {
				rm.derivativeMarks[symbol] = previous
			} else {
				delete(rm.derivativeMarks, symbol)
			}
		}
	}()
	updates := make([]settledDerivativePosition, 0)
	for clientID, positions := range rm.derivativePositions {
		position, exists := positions[symbol]
		if !exists {
			continue
		}
		pnl, err := positionPnL(position, rm.derivativeLots[clientID][symbol], price, instrument.ContractMultiplier)
		if err != nil {
			return err
		}
		position.RealizedPnL, err = addMoney(position.RealizedPnL, pnl)
		if err != nil {
			return err
		}
		position.UnrealizedPnL = 0
		var lots []DerivativeLot
		if position.NetContracts != 0 {
			lots = []DerivativeLot{{Quantity: absoluteContracts(position.NetContracts), EntryPrice: price}}
		}
		if _, err := rm.calculateMarginAccountLocked(clientID, symbol, &position, lots); err != nil {
			return err
		}
		updates = append(updates, settledDerivativePosition{clientID: clientID, position: position, lots: lots})
	}
	for _, update := range updates {
		rm.derivativePositions[update.clientID][symbol] = update.position
		rm.storeDerivativeLotsLocked(update.clientID, symbol, update.lots)
	}
	committed = true
	return nil
}

func (rm *RiskManager) accountsWithMarginShortfall() map[uuid.UUID]bool {
	result := make(map[uuid.UUID]bool)
	if !rm.derivativesEnabled {
		return result
	}
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	for clientID := range rm.clients {
		account, err := rm.calculateMarginAccountLocked(clientID, "", nil, nil)
		if err == nil && account.InitialMarginShortfall > 0 {
			result[clientID] = true
		}
	}
	return result
}
