package matching

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/google/uuid"
)

type DerivativeInstrument struct {
	Symbol                       string
	MarginCurrency               string
	InitialMarginPerContract     uint64
	MaintenanceMarginPerContract uint64
	ContractMultiplier           uint64
}

type DerivativePosition struct {
	NetContracts      int64
	OpenBuyContracts  uint64
	OpenSellContracts uint64
	RealizedPnL       int64
	UnrealizedPnL     int64
}

type MarginAccount struct {
	Collateral                 uint64
	PositionMargin             uint64
	OrderMargin                uint64
	AvailableCollateral        uint64
	RealizedPnL                int64
	UnrealizedPnL              int64
	Equity                     int64
	MaintenanceMargin          uint64
	InitialMarginShortfall     uint64
	MaintenanceMarginShortfall uint64
}

type derivativeOrderKey struct {
	Symbol  string
	OrderID uuid.UUID
}

type derivativeReservation struct {
	ClientID  uuid.UUID
	Side      Side
	Remaining uint64
}

type marginRequirement struct {
	Position         uint64
	Total            uint64
	MaximumContracts uint64
}

var (
	ErrUnknownMarkPrice                = errors.New("mark price must be positive and known")
	ErrInsufficientMargin              = errors.New("insufficient available margin")
	ErrMarginCurrencyMismatch          = errors.New("margin currency does not match the account")
	ErrUnknownDerivativeInstrument     = errors.New("unknown derivative instrument")
	ErrInvalidDerivativeInstrument     = errors.New("invalid derivative instrument")
	ErrPositionLimitExceeded           = errors.New("potential position exceeds the position limit")
	ErrRiskArithmeticOverflow          = errors.New("risk arithmetic exceeds the supported range")
	ErrInvalidDerivativeTrade          = errors.New("trade does not match reserved derivative orders")
	ErrDerivativeTradeRequiresMatching = errors.New("derivative positions must be updated by the matching engine")
	ErrDerivativeSelfTrade             = errors.New("self-trading is not allowed for derivatives")
)

func (rm *RiskManager) addDerivativeInstrument(instrument DerivativeInstrument) error {
	if !rm.derivativesEnabled {
		return errors.New("derivative instruments require a derivatives engine")
	}
	if instrument.ContractMultiplier == 0 {
		instrument.ContractMultiplier = 1
	}
	if instrument.MaintenanceMarginPerContract == 0 {
		instrument.MaintenanceMarginPerContract = instrument.InitialMarginPerContract
	}
	if instrument.ContractMultiplier > math.MaxInt64 || instrument.MaintenanceMarginPerContract > instrument.InitialMarginPerContract {
		return ErrInvalidDerivativeInstrument
	}
	if instrument.Symbol == "" || strings.TrimSpace(instrument.Symbol) != instrument.Symbol || instrument.MarginCurrency == "" || strings.TrimSpace(instrument.MarginCurrency) != instrument.MarginCurrency || instrument.InitialMarginPerContract == 0 {
		return ErrInvalidDerivativeInstrument
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	if previous, exists := rm.derivativeInstruments[instrument.Symbol]; exists && previous != instrument {
		return fmt.Errorf("%w: configured margin cannot be changed in this increment", ErrInvalidDerivativeInstrument)
	}
	rm.derivativeInstruments[instrument.Symbol] = instrument
	return nil
}

func changeNetContracts(current int64, quantity uint64, side Side) (int64, error) {
	if quantity > math.MaxInt64 || current == math.MinInt64 {
		return 0, ErrRiskArithmeticOverflow
	}
	change := int64(quantity)
	if side == Bid {
		if current > math.MaxInt64-change {
			return 0, ErrRiskArithmeticOverflow
		}
		return current + change, nil
	}
	if current < -math.MaxInt64+change {
		return 0, ErrRiskArithmeticOverflow
	}
	return current - change, nil
}

func absoluteContracts(contracts int64) uint64 {
	if contracts < 0 {
		return uint64(-contracts)
	}
	return uint64(contracts)
}

func calculatePositionMargin(position DerivativePosition, rate uint64) (marginRequirement, error) {
	maximumLong, err := changeNetContracts(position.NetContracts, position.OpenBuyContracts, Bid)
	if err != nil {
		return marginRequirement{}, err
	}
	maximumShort, err := changeNetContracts(position.NetContracts, position.OpenSellContracts, Ask)
	if err != nil {
		return marginRequirement{}, err
	}
	contracts := max(absoluteContracts(maximumLong), absoluteContracts(maximumShort))
	if rate == 0 || contracts > math.MaxUint64/rate {
		return marginRequirement{}, ErrRiskArithmeticOverflow
	}
	return marginRequirement{Position: absoluteContracts(position.NetContracts) * rate, Total: contracts * rate, MaximumContracts: contracts}, nil
}

func addMarginRequirement(account *MarginAccount, requirement marginRequirement) error {
	orderMargin := requirement.Total - requirement.Position
	if account.PositionMargin > math.MaxUint64-requirement.Position || account.OrderMargin > math.MaxUint64-orderMargin {
		return ErrRiskArithmeticOverflow
	}
	account.PositionMargin += requirement.Position
	account.OrderMargin += orderMargin
	return nil
}

func (rm *RiskManager) calculateMarginAccountLocked(clientID uuid.UUID, replacementSymbol string, replacement *DerivativePosition, replacementLots []DerivativeLot) (MarginAccount, error) {
	client, exists := rm.clients[clientID]
	if !exists {
		return MarginAccount{}, ErrClientNotFound
	}
	if client.Collateral > math.MaxInt64 {
		return MarginAccount{}, ErrRiskArithmeticOverflow
	}
	account := MarginAccount{Collateral: client.Collateral}
	totals := pnlTotals{}
	for symbol, position := range rm.derivativePositions[clientID] {
		if replacement != nil && symbol == replacementSymbol {
			continue
		}
		if err := rm.addValuedPositionLocked(&account, symbol, position, rm.derivativeLots[clientID][symbol], &totals); err != nil {
			return MarginAccount{}, err
		}
	}
	if replacement != nil {
		if err := rm.addValuedPositionLocked(&account, replacementSymbol, *replacement, replacementLots, &totals); err != nil {
			return MarginAccount{}, err
		}
	}
	if account.PositionMargin > math.MaxUint64-account.OrderMargin {
		return MarginAccount{}, ErrRiskArithmeticOverflow
	}
	var err error
	account.RealizedPnL, err = signedMoney(&totals.realized)
	if err != nil {
		return MarginAccount{}, err
	}
	account.UnrealizedPnL, err = signedMoney(&totals.unrealized)
	if err != nil {
		return MarginAccount{}, err
	}
	equity := big.NewInt(int64(account.Collateral))
	equity.Add(equity, &totals.realized)
	equity.Add(equity, &totals.unrealized)
	account.Equity, err = signedMoney(equity)
	if err != nil {
		return MarginAccount{}, err
	}
	required := account.PositionMargin + account.OrderMargin
	if required > math.MaxInt64 {
		return MarginAccount{}, ErrRiskArithmeticOverflow
	}
	account.InitialMarginShortfall = marginShortfall(required, account.Equity)
	account.MaintenanceMarginShortfall = marginShortfall(account.MaintenanceMargin, account.Equity)
	if account.Equity > 0 && uint64(account.Equity) > required {
		account.AvailableCollateral = uint64(account.Equity) - required
	}
	return account, nil
}

func (rm *RiskManager) checkDerivativeOrderLocked(order *Order, reserve bool) error {
	instrument, exists := rm.derivativeInstruments[order.Symbol]
	if !exists {
		return ErrUnknownDerivativeInstrument
	}
	client, exists := rm.clients[order.ClientID]
	if !exists {
		return ErrClientNotFound
	}
	if client.MarginCurrency != instrument.MarginCurrency {
		return ErrMarginCurrencyMismatch
	}
	key := derivativeOrderKey{Symbol: order.Symbol, OrderID: order.ID}
	if _, exists := rm.derivativeReservations[key]; exists {
		return errors.New("order already has a margin reservation")
	}
	currentPosition := rm.derivativePositions[order.ClientID][order.Symbol]
	position := currentPosition
	if order.Remaining > math.MaxInt64 {
		return ErrRiskArithmeticOverflow
	}
	if order.Side == Bid {
		if position.OpenBuyContracts > math.MaxInt64-order.Remaining {
			return ErrRiskArithmeticOverflow
		}
		position.OpenBuyContracts += order.Remaining
	} else {
		if position.OpenSellContracts > math.MaxInt64-order.Remaining {
			return ErrRiskArithmeticOverflow
		}
		position.OpenSellContracts += order.Remaining
	}
	requirement, err := calculatePositionMargin(position, instrument.InitialMarginPerContract)
	if err != nil {
		return err
	}
	rm.limitsMu.RLock()
	limits, hasLimits := rm.riskLimits[order.ClientID]
	rm.limitsMu.RUnlock()
	if hasLimits && !isClosingMarketOrder(order, currentPosition) {
		if limits.MaxPositionSize > 0 && requirement.MaximumContracts > limits.MaxPositionSize {
			return ErrPositionLimitExceeded
		}
		pending := position.OpenBuyContracts + position.OpenSellContracts
		rm.volumeMu.RLock()
		filled := rm.dailyVolume[order.ClientID][order.Symbol]
		rm.volumeMu.RUnlock()
		if filled > math.MaxUint64-pending || filled+pending > limits.MaxDailyVolume {
			return errors.New("daily volume limit includes pending derivative orders")
		}
	}
	account, err := rm.calculateMarginAccountLocked(order.ClientID, order.Symbol, &position, rm.derivativeLots[order.ClientID][order.Symbol])
	if err != nil {
		return err
	}
	if account.InitialMarginShortfall > 0 && !isClosingMarketOrder(order, currentPosition) {
		return ErrInsufficientMargin
	}
	if reserve {
		if rm.derivativePositions[order.ClientID] == nil {
			rm.derivativePositions[order.ClientID] = make(map[string]DerivativePosition)
		}
		rm.derivativePositions[order.ClientID][order.Symbol] = position
		rm.derivativeReservations[key] = derivativeReservation{ClientID: order.ClientID, Side: order.Side, Remaining: order.Remaining}
	}
	return nil
}

func (rm *RiskManager) releaseDerivativeOrder(orderID uuid.UUID, symbol string) {
	if !rm.derivativesEnabled {
		return
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	rm.releaseDerivativeOrderLocked(derivativeOrderKey{Symbol: symbol, OrderID: orderID})
}

func (rm *RiskManager) releaseDerivativeOrderLocked(key derivativeOrderKey) {
	reservation, exists := rm.derivativeReservations[key]
	if !exists {
		return
	}
	position := rm.derivativePositions[reservation.ClientID][key.Symbol]
	if reservation.Side == Bid {
		position.OpenBuyContracts -= reservation.Remaining
	} else {
		position.OpenSellContracts -= reservation.Remaining
	}
	rm.derivativePositions[reservation.ClientID][key.Symbol] = position
	delete(rm.derivativeReservations, key)
}

func (rm *RiskManager) releasePendingDerivativeOrders() {
	if !rm.derivativesEnabled {
		return
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	for key := range rm.derivativeReservations {
		rm.releaseDerivativeOrderLocked(key)
	}
}

func (rm *RiskManager) settleTrade(trade *Trade) error {
	if !rm.derivativesEnabled {
		return rm.UpdatePosition(trade)
	}
	if trade == nil || trade.Quantity == 0 || trade.Price == 0 {
		return ErrInvalidDerivativeTrade
	}
	if trade.BidClient == trade.AskClient {
		return ErrDerivativeSelfTrade
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	bidKey := derivativeOrderKey{Symbol: trade.Symbol, OrderID: trade.BidOrder}
	askKey := derivativeOrderKey{Symbol: trade.Symbol, OrderID: trade.AskOrder}
	bidReservation, bidExists := rm.derivativeReservations[bidKey]
	askReservation, askExists := rm.derivativeReservations[askKey]
	if !bidExists || !askExists || bidReservation.ClientID != trade.BidClient || askReservation.ClientID != trade.AskClient || bidReservation.Side != Bid || askReservation.Side != Ask || bidReservation.Remaining < trade.Quantity || askReservation.Remaining < trade.Quantity {
		return ErrInvalidDerivativeTrade
	}
	bidPosition := rm.derivativePositions[trade.BidClient][trade.Symbol]
	askPosition := rm.derivativePositions[trade.AskClient][trade.Symbol]
	bidPosition, bidLots, err := applyDerivativeFill(bidPosition, rm.derivativeLots[trade.BidClient][trade.Symbol], Bid, trade.Quantity, trade.Price, rm.derivativeInstruments[trade.Symbol].ContractMultiplier)
	if err != nil {
		return err
	}
	askPosition, askLots, err := applyDerivativeFill(askPosition, rm.derivativeLots[trade.AskClient][trade.Symbol], Ask, trade.Quantity, trade.Price, rm.derivativeInstruments[trade.Symbol].ContractMultiplier)
	if err != nil {
		return err
	}
	bidPosition.OpenBuyContracts -= trade.Quantity
	askPosition.OpenSellContracts -= trade.Quantity
	_, hadMark := rm.derivativeMarks[trade.Symbol]
	if !hadMark {
		rm.derivativeMarks[trade.Symbol] = trade.Price
	}
	committed := false
	defer func() {
		if !committed && !hadMark {
			delete(rm.derivativeMarks, trade.Symbol)
		}
	}()
	if _, err := rm.calculateMarginAccountLocked(trade.BidClient, trade.Symbol, &bidPosition, bidLots); err != nil {
		return err
	}
	if _, err := rm.calculateMarginAccountLocked(trade.AskClient, trade.Symbol, &askPosition, askLots); err != nil {
		return err
	}
	rm.volumeMu.Lock()
	defer rm.volumeMu.Unlock()
	bidVolume := rm.dailyVolume[trade.BidClient][trade.Symbol]
	askVolume := rm.dailyVolume[trade.AskClient][trade.Symbol]
	if bidVolume > math.MaxUint64-trade.Quantity || askVolume > math.MaxUint64-trade.Quantity {
		return ErrRiskArithmeticOverflow
	}
	rm.derivativePositions[trade.BidClient][trade.Symbol] = bidPosition
	rm.derivativePositions[trade.AskClient][trade.Symbol] = askPosition
	rm.storeDerivativeLotsLocked(trade.BidClient, trade.Symbol, bidLots)
	rm.storeDerivativeLotsLocked(trade.AskClient, trade.Symbol, askLots)
	bidReservation.Remaining -= trade.Quantity
	askReservation.Remaining -= trade.Quantity
	if bidReservation.Remaining == 0 {
		delete(rm.derivativeReservations, bidKey)
	} else {
		rm.derivativeReservations[bidKey] = bidReservation
	}
	if askReservation.Remaining == 0 {
		delete(rm.derivativeReservations, askKey)
	} else {
		rm.derivativeReservations[askKey] = askReservation
	}
	rm.dailyVolume[trade.BidClient][trade.Symbol] = bidVolume + trade.Quantity
	rm.dailyVolume[trade.AskClient][trade.Symbol] = askVolume + trade.Quantity
	committed = true
	return nil
}

func (rm *RiskManager) GetDerivativePosition(clientID uuid.UUID, symbol string) (DerivativePosition, bool) {
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	position, exists := rm.derivativePositions[clientID][symbol]
	if exists {
		pnl, err := positionPnL(position, rm.derivativeLots[clientID][symbol], rm.derivativeMarks[symbol], rm.derivativeInstruments[symbol].ContractMultiplier)
		if err != nil {
			return DerivativePosition{}, false
		}
		position.UnrealizedPnL = pnl
	}
	return position, exists
}

func (rm *RiskManager) GetMarginAccount(clientID uuid.UUID) (MarginAccount, bool) {
	if !rm.derivativesEnabled {
		return MarginAccount{}, false
	}
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	account, err := rm.calculateMarginAccountLocked(clientID, "", nil, nil)
	return account, err == nil
}
