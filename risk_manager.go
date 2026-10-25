package main

import (
	"errors"
	"math"
	"math/bits"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidRiskLimits     = errors.New("risk limits require a client ID and non-nil limits")
	ErrInvalidCircuitBreaker = errors.New("invalid circuit breaker configuration")
)

type RiskManager struct {
	derivativesEnabled     bool
	derivativeInstruments  map[string]DerivativeInstrument
	derivativePositions    map[uuid.UUID]map[string]DerivativePosition
	derivativeReservations map[derivativeOrderKey]derivativeReservation
	derivativeLots         map[uuid.UUID]map[string][]DerivativeLot
	derivativeMarks        map[string]uint64
	stop                   chan struct{}
	done                   chan struct{}
	closeOnce              sync.Once
	clients                map[uuid.UUID]*Client
	accountMu              sync.RWMutex
	riskLimits             map[uuid.UUID]*RiskLimits
	limitsMu               sync.RWMutex
	circuitBreakers        map[string]*CircuitBreaker
	breakerMu              sync.RWMutex
	dailyVolume            map[uuid.UUID]map[string]uint64
	volumeMu               sync.RWMutex
	statistics             RiskStats
	statsMu                sync.RWMutex
}

// RiskStats represents risk management statistics
type RiskStats struct {
	OrdersRejected     uint64
	OrdersBlockedByCB  uint64
	ExposureBreaches   uint64
	CollateralBreaches uint64
}

// NewRiskManager creates a new risk manager
func NewRiskManager() *RiskManager { return newRiskManager(false) }

func newRiskManager(derivativesEnabled bool) *RiskManager {
	return newRiskManagerMode(derivativesEnabled, true)
}

func newRiskManagerMode(derivativesEnabled, background bool) *RiskManager {
	rm := &RiskManager{
		derivativesEnabled:     derivativesEnabled,
		derivativeInstruments:  make(map[string]DerivativeInstrument),
		derivativePositions:    make(map[uuid.UUID]map[string]DerivativePosition),
		derivativeReservations: make(map[derivativeOrderKey]derivativeReservation),
		derivativeLots:         make(map[uuid.UUID]map[string][]DerivativeLot),
		derivativeMarks:        make(map[string]uint64),
		stop:                   make(chan struct{}),
		done:                   make(chan struct{}),
		clients:                make(map[uuid.UUID]*Client),
		riskLimits:             make(map[uuid.UUID]*RiskLimits),
		circuitBreakers:        make(map[string]*CircuitBreaker),
		dailyVolume:            make(map[uuid.UUID]map[string]uint64),
	}

	// Start circuit breaker cooldown monitor
	if background {
		go rm.cooldownMonitor()
	} else {
		close(rm.done)
	}

	return rm
}

// AddClient adds a new client to the risk manager
func (rm *RiskManager) AddClient(client *Client) error {
	if client == nil || client.ID == uuid.Nil {
		return ErrClientNotFound
	}
	copied := *client
	if rm.derivativesEnabled && copied.Collateral > math.MaxInt64 {
		return ErrRiskArithmeticOverflow
	}
	if rm.derivativesEnabled && (copied.MarginCurrency == "" || strings.TrimSpace(copied.MarginCurrency) != copied.MarginCurrency) {
		return ErrMarginCurrencyMismatch
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()
	if rm.derivativesEnabled {
		account, err := rm.calculateMarginAccountLocked(copied.ID, "", nil, nil)
		if err != nil && !errors.Is(err, ErrClientNotFound) {
			return err
		}
		if previous := rm.clients[copied.ID]; previous != nil {
			candidateEquity, err := addMoney(account.Equity, int64(copied.Collateral)-int64(previous.Collateral))
			if err != nil {
				return err
			}
			if copied.Collateral < previous.Collateral && marginShortfall(account.PositionMargin+account.OrderMargin, candidateEquity) > 0 {
				return ErrInsufficientMargin
			}
		}
		if previous := rm.clients[copied.ID]; previous != nil && previous.MarginCurrency != copied.MarginCurrency {
			return ErrMarginCurrencyMismatch
		}
	}
	rm.clients[copied.ID] = &copied
	rm.volumeMu.Lock()
	defer rm.volumeMu.Unlock()
	if _, exists := rm.dailyVolume[copied.ID]; !exists {
		rm.dailyVolume[copied.ID] = make(map[string]uint64)
	}
	return nil
}

// SetRiskLimits sets risk limits for a client
func (rm *RiskManager) SetRiskLimits(clientID uuid.UUID, limits *RiskLimits) error {
	if clientID == uuid.Nil || limits == nil {
		return ErrInvalidRiskLimits
	}
	rm.limitsMu.Lock()
	defer rm.limitsMu.Unlock()
	copied := *limits
	rm.riskLimits[clientID] = &copied
	return nil
}

// AddCircuitBreaker adds a circuit breaker for a symbol
func (rm *RiskManager) AddCircuitBreaker(breaker *CircuitBreaker) error {
	if breaker == nil || breaker.Symbol == "" || strings.TrimSpace(breaker.Symbol) != breaker.Symbol || breaker.ThresholdPct == 0 || breaker.CooldownPeriod < 0 || breaker.State < CircuitBreakerClosed || breaker.State > CircuitBreakerHalfOpen {
		return ErrInvalidCircuitBreaker
	}
	rm.breakerMu.Lock()
	defer rm.breakerMu.Unlock()
	copied := *breaker
	rm.circuitBreakers[breaker.Symbol] = &copied
	return nil
}

// CheckOrder performs all risk checks before allowing an order
func (rm *RiskManager) CheckOrder(order *Order) RiskCheckResult {
	return rm.checkOrder(order, false)
}

func (rm *RiskManager) checkAndReserveOrder(order *Order) RiskCheckResult {
	return rm.checkOrder(order, true)
}

func (rm *RiskManager) checkOrder(order *Order, reserve bool) RiskCheckResult {
	if order == nil {
		return RiskCheckResult{Reason: ErrInvalidOrder.Error()}
	}
	if rm.derivativesEnabled {
		if err := validateNewOrder(*order); err != nil {
			return RiskCheckResult{Reason: err.Error()}
		}
	}

	// Check circuit breaker first
	if cbResult := rm.checkCircuitBreaker(order); !cbResult.Approved {
		return cbResult
	}

	// Check order size limits
	if sizeResult := rm.checkOrderSize(order); !sizeResult.Approved {
		return sizeResult
	}

	if rm.derivativesEnabled {
		rm.accountMu.Lock()
		err := rm.checkDerivativeOrderLocked(order, reserve)
		rm.accountMu.Unlock()
		if err != nil {
			rm.statsMu.Lock()
			rm.statistics.OrdersRejected++
			rm.statsMu.Unlock()
			return RiskCheckResult{Reason: err.Error()}
		}
		return RiskCheckResult{Approved: true}
	}

	// Check daily volume limits
	if volumeResult := rm.checkDailyVolume(order); !volumeResult.Approved {
		return volumeResult
	}

	// Check exposure limits
	if exposureResult := rm.checkExposure(order); !exposureResult.Approved {
		return exposureResult
	}

	// Check collateral/margin
	if collateralResult := rm.checkCollateral(order); !collateralResult.Approved {
		return collateralResult
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// checkCircuitBreaker checks if trading is halted for the symbol
func (rm *RiskManager) checkCircuitBreaker(order *Order) RiskCheckResult {
	rm.breakerMu.RLock()
	breaker, exists := rm.circuitBreakers[order.Symbol]
	if exists {
		copied := *breaker
		breaker = &copied
	}
	rm.breakerMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	if breaker.State == CircuitBreakerOpen {
		rm.statsMu.Lock()
		rm.statistics.OrdersBlockedByCB++
		rm.statsMu.Unlock()
		return RiskCheckResult{
			Approved: false,
			Reason:   "circuit breaker is open for symbol",
		}
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// checkOrderSize checks if order size exceeds limits
func (rm *RiskManager) checkOrderSize(order *Order) RiskCheckResult {
	rm.limitsMu.RLock()
	limits, exists := rm.riskLimits[order.ClientID]
	rm.limitsMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	if order.Quantity > limits.MaxOrderSize {
		rm.statsMu.Lock()
		rm.statistics.OrdersRejected++
		rm.statsMu.Unlock()
		return RiskCheckResult{
			Approved: false,
			Reason:   "order size exceeds maximum allowed",
		}
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// checkDailyVolume checks if daily volume limit is exceeded
func (rm *RiskManager) checkDailyVolume(order *Order) RiskCheckResult {
	rm.limitsMu.RLock()
	limits, exists := rm.riskLimits[order.ClientID]
	rm.limitsMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	rm.volumeMu.RLock()
	volumeMap := rm.dailyVolume[order.ClientID]
	currentVolume := volumeMap[order.Symbol]
	rm.volumeMu.RUnlock()

	if currentVolume+order.Quantity > limits.MaxDailyVolume {
		rm.statsMu.Lock()
		rm.statistics.OrdersRejected++
		rm.statsMu.Unlock()
		return RiskCheckResult{
			Approved: false,
			Reason:   "daily volume limit exceeded",
		}
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// checkExposure checks if exposure limits are exceeded
func (rm *RiskManager) checkExposure(order *Order) RiskCheckResult {
	rm.accountMu.RLock()
	client, exists := rm.clients[order.ClientID]
	if exists {
		copied := *client
		client = &copied
	}
	rm.accountMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	rm.limitsMu.RLock()
	limits, exists := rm.riskLimits[order.ClientID]
	rm.limitsMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	// Calculate potential exposure after this order
	potentialExposure := client.Exposure
	if order.Side == Bid {
		potentialExposure += order.Quantity * order.Price
	} else {
		potentialExposure -= order.Quantity * order.Price
	}

	maxExposure := (client.Collateral * limits.MaxExposure) / 100
	if potentialExposure > maxExposure {
		rm.statsMu.Lock()
		rm.statistics.ExposureBreaches++
		rm.statsMu.Unlock()
		return RiskCheckResult{
			Approved: false,
			Reason:   "exposure limit exceeded",
		}
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// checkCollateral checks if sufficient collateral is available
func (rm *RiskManager) checkCollateral(order *Order) RiskCheckResult {
	rm.accountMu.RLock()
	client, exists := rm.clients[order.ClientID]
	if exists {
		copied := *client
		client = &copied
	}
	rm.accountMu.RUnlock()

	if !exists {
		return RiskCheckResult{Approved: true, Reason: ""}
	}

	// For buy orders, check if collateral covers the order value
	if order.Side == Bid {
		orderValue := order.Quantity * order.Price
		if orderValue > client.Collateral {
			rm.statsMu.Lock()
			rm.statistics.CollateralBreaches++
			rm.statsMu.Unlock()
			return RiskCheckResult{
				Approved: false,
				Reason:   "insufficient collateral",
			}
		}
	}

	return RiskCheckResult{Approved: true, Reason: ""}
}

// UpdatePosition updates client position after a trade
func (rm *RiskManager) UpdatePosition(trade *Trade) error {
	if rm.derivativesEnabled {
		return ErrDerivativeTradeRequiresMatching
	}
	if trade == nil {
		return ErrInvalidDerivativeTrade
	}
	rm.accountMu.Lock()
	defer rm.accountMu.Unlock()

	// Update bid client position
	if bidClient, exists := rm.clients[trade.BidClient]; exists {
		bidClient.LongPosition += trade.Quantity
		bidClient.Exposure += trade.Quantity * trade.Price
	}

	// Update ask client position
	if askClient, exists := rm.clients[trade.AskClient]; exists {
		askClient.ShortPosition += trade.Quantity
		askClient.Exposure -= trade.Quantity * trade.Price
	}

	// Update daily volume
	rm.volumeMu.Lock()
	defer rm.volumeMu.Unlock()

	if volMap, exists := rm.dailyVolume[trade.BidClient]; exists {
		volMap[trade.Symbol] += trade.Quantity
	}
	if volMap, exists := rm.dailyVolume[trade.AskClient]; exists {
		volMap[trade.Symbol] += trade.Quantity
	}
	return nil
}

// CheckCircuitBreakerTrigger checks if a circuit breaker should be triggered
func (rm *RiskManager) CheckCircuitBreakerTrigger(symbol string, price uint64) {
	rm.breakerMu.Lock()
	defer rm.breakerMu.Unlock()

	breaker, exists := rm.circuitBreakers[symbol]
	if !exists {
		return
	}

	if breaker.State == CircuitBreakerOpen {
		return // Already open
	}

	// Calculate percentage change from last price
	if breaker.LastPrice == 0 {
		breaker.LastPrice = price
		return
	}

	change := absDiff(price, breaker.LastPrice)
	changeHigh, changeLow := bits.Mul64(change, 100)
	thresholdHigh, thresholdLow := bits.Mul64(breaker.LastPrice, breaker.ThresholdPct)
	if changeHigh > thresholdHigh || (changeHigh == thresholdHigh && changeLow >= thresholdLow) {
		// Trigger circuit breaker
		breaker.State = CircuitBreakerOpen
		breaker.TriggerPrice = price
		breaker.OpenTime = time.Now().UnixNano()
	}

	breaker.LastPrice = price
}

// cooldownMonitor periodically checks if circuit breakers can be reopened
func (rm *RiskManager) cooldownMonitor() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	defer close(rm.done)
	for {
		select {
		case <-rm.stop:
			return
		case <-ticker.C:
			rm.checkCooldowns()
		}
	}
}

// checkCooldowns checks if any circuit breakers can be reopened
func (rm *RiskManager) checkCooldowns() {
	rm.breakerMu.Lock()
	defer rm.breakerMu.Unlock()

	now := time.Now().UnixNano()

	for _, breaker := range rm.circuitBreakers {
		if breaker.State == CircuitBreakerOpen {
			if now-breaker.OpenTime >= breaker.CooldownPeriod {
				// Reopen circuit breaker
				breaker.State = CircuitBreakerClosed
				breaker.OpenTime = 0
			}
		}
	}
}

// GetClient returns a client by ID
func (rm *RiskManager) GetClient(clientID uuid.UUID) (*Client, bool) {
	rm.accountMu.RLock()
	defer rm.accountMu.RUnlock()
	client, exists := rm.clients[clientID]
	if !exists {
		return nil, false
	}
	copied := *client
	return &copied, true
}

// GetCircuitBreaker returns the circuit breaker for a symbol
func (rm *RiskManager) GetCircuitBreaker(symbol string) (*CircuitBreaker, bool) {
	rm.breakerMu.RLock()
	defer rm.breakerMu.RUnlock()
	breaker, exists := rm.circuitBreakers[symbol]
	if !exists {
		return nil, false
	}
	copied := *breaker
	return &copied, true
}

// GetStats returns risk management statistics
func (rm *RiskManager) GetStats() RiskStats {
	rm.statsMu.RLock()
	defer rm.statsMu.RUnlock()
	return RiskStats{
		OrdersRejected:     rm.statistics.OrdersRejected,
		OrdersBlockedByCB:  rm.statistics.OrdersBlockedByCB,
		ExposureBreaches:   rm.statistics.ExposureBreaches,
		CollateralBreaches: rm.statistics.CollateralBreaches,
	}
}

// ResetDailyVolume resets daily volume for a client (called at start of day)
func (rm *RiskManager) ResetDailyVolume(clientID uuid.UUID) {
	rm.volumeMu.Lock()
	defer rm.volumeMu.Unlock()
	rm.dailyVolume[clientID] = make(map[string]uint64)
}

// absDiff calculates absolute difference between two values
func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

var ErrClientNotFound = errors.New("client not found")
var ErrInsufficientCollateral = errors.New("insufficient collateral")
var ErrExposureLimitExceeded = errors.New("exposure limit exceeded")
var ErrCircuitBreakerOpen = errors.New("circuit breaker is open")

func (rm *RiskManager) Close() {
	rm.closeOnce.Do(func() { close(rm.stop) })
	<-rm.done
}
