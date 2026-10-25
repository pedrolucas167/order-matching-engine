package matching

import (
	"github.com/google/uuid"
)

// Side represents buy or sell side
type Side int

const (
	Bid Side = iota
	Ask
)

type OrderType int

const (
	Limit OrderType = iota
	Market
	StopMarket
	StopLimit
)

type OrderStatus int

const (
	Pending OrderStatus = iota
	Open
	PartialFilled
	Filled
	Cancelled
	Rejected
	Triggered
)

type Order struct {
	ID        uuid.UUID
	ClientID  uuid.UUID
	Symbol    string
	Side      Side
	Type      OrderType
	Price     uint64
	StopPrice uint64
	Quantity  uint64
	Remaining uint64
	Status    OrderStatus
	Timestamp int64
}

type Trade struct {
	ID        uuid.UUID
	Symbol    string
	Price     uint64
	Quantity  uint64
	BidOrder  uuid.UUID
	AskOrder  uuid.UUID
	BidClient uuid.UUID
	AskClient uuid.UUID
	Timestamp int64
}

type OrderBookSnapshot struct {
	Symbol    string
	BestBid   *PriceLevel
	BestAsk   *PriceLevel
	Timestamp int64
}

type PriceLevel struct {
	Price     uint64
	TotalQty  uint64
	Orders    []*Order
	Timestamp int64
}

type Client struct {
	ID             uuid.UUID
	Name           string
	Collateral     uint64
	MarginCurrency string
	Exposure       uint64
	LongPosition   uint64
	ShortPosition  uint64
}

type RiskLimits struct {
	MaxOrderSize    uint64
	MaxDailyVolume  uint64
	MaxExposure     uint64
	MaxPositionSize uint64
}

type CircuitBreakerState int

const (
	CircuitBreakerClosed CircuitBreakerState = iota
	CircuitBreakerOpen
	CircuitBreakerHalfOpen
)

type CircuitBreaker struct {
	Symbol         string
	State          CircuitBreakerState
	TriggerPrice   uint64
	ThresholdPct   uint64
	LastPrice      uint64
	OpenTime       int64
	CooldownPeriod int64
}

type RiskCheckResult struct {
	Approved bool
	Reason   string
}
