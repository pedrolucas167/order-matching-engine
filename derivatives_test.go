package main

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func newDerivativeRiskTest(t *testing.T, rate, collateral uint64) (*RiskManager, uuid.UUID, uuid.UUID) {
	t.Helper()
	manager := newRiskManager(true)
	t.Cleanup(manager.Close)
	if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: rate}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := manager.AddClient(&Client{ID: id, Collateral: collateral, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	return manager, buyer, seller
}

func derivativeTestOrder(clientID uuid.UUID, side Side, quantity uint64) *Order {
	order := testOrder(side, Limit, quantity)
	order.Symbol = "SIM-FUT"
	order.ClientID = clientID
	order.Price = 10000
	return order
}

func reserveDerivativeTestOrder(t *testing.T, manager *RiskManager, order *Order) {
	t.Helper()
	if result := manager.checkAndReserveOrder(order); !result.Approved {
		t.Fatalf("reservation rejected: %s", result.Reason)
	}
}

func TestDerivativeRiskPreviewDoesNotReserve(t *testing.T) {
	manager, _, seller := newDerivativeRiskTest(t, 100, 200)
	order := derivativeTestOrder(seller, Ask, 2)
	for i := 0; i < 2; i++ {
		if result := manager.CheckOrder(order); !result.Approved {
			t.Fatal(result.Reason)
		}
	}
	account, _ := manager.GetMarginAccount(seller)
	if account.AvailableCollateral != 200 || account.OrderMargin != 0 {
		t.Fatalf("preview reserved margin: %+v", account)
	}
	reserveDerivativeTestOrder(t, manager, order)
	account, _ = manager.GetMarginAccount(seller)
	if account.AvailableCollateral != 0 || account.OrderMargin != 200 {
		t.Fatalf("short order margin: %+v", account)
	}
	position, _ := manager.GetDerivativePosition(seller, "SIM-FUT")
	if position.NetContracts != 0 || position.OpenSellContracts != 2 {
		t.Fatalf("unexpected pending short: %+v", position)
	}
}

func TestConcurrentDerivativeReservationsCannotReuseMargin(t *testing.T) {
	manager, buyer, _ := newDerivativeRiskTest(t, 100, 100)
	var approved atomic.Int64
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if manager.checkAndReserveOrder(derivativeTestOrder(buyer, Bid, 1)).Approved {
				approved.Add(1)
			}
		}()
	}
	workers.Wait()
	if approved.Load() != 1 {
		t.Fatalf("approved %d orders for one contract of collateral", approved.Load())
	}
	account, _ := manager.GetMarginAccount(buyer)
	if account.OrderMargin != 100 || account.AvailableCollateral != 0 {
		t.Fatalf("collateral was reused: %+v", account)
	}
}

func TestDerivativePartialFillsAndClosingReleaseMargin(t *testing.T) {
	manager, buyer, seller := newDerivativeRiskTest(t, 100, 300)
	bid, ask := derivativeTestOrder(buyer, Bid, 2), derivativeTestOrder(seller, Ask, 2)
	reserveDerivativeTestOrder(t, manager, bid)
	reserveDerivativeTestOrder(t, manager, ask)
	trade := &Trade{ID: uuid.New(), Symbol: "SIM-FUT", Quantity: 1, Price: 10000, BidOrder: bid.ID, AskOrder: ask.ID, BidClient: buyer, AskClient: seller}
	if err := manager.settleTrade(trade); err != nil {
		t.Fatal(err)
	}
	buyerPosition, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	sellerPosition, _ := manager.GetDerivativePosition(seller, "SIM-FUT")
	if buyerPosition.NetContracts != 1 || sellerPosition.NetContracts != -1 || buyerPosition.OpenBuyContracts != 1 || sellerPosition.OpenSellContracts != 1 {
		t.Fatalf("partial positions: %+v %+v", buyerPosition, sellerPosition)
	}
	for _, id := range []uuid.UUID{buyer, seller} {
		account, _ := manager.GetMarginAccount(id)
		if account.PositionMargin != 100 || account.OrderMargin != 100 || account.AvailableCollateral != 100 {
			t.Fatalf("partial margin: %+v", account)
		}
	}
	manager.releaseDerivativeOrder(bid.ID, bid.Symbol)
	manager.releaseDerivativeOrder(ask.ID, ask.Symbol)
	for _, id := range []uuid.UUID{buyer, seller} {
		account, _ := manager.GetMarginAccount(id)
		if account.PositionMargin != 100 || account.OrderMargin != 0 || account.AvailableCollateral != 200 {
			t.Fatalf("cancelled remainder margin: %+v", account)
		}
	}
	closeBid, closeAsk := derivativeTestOrder(seller, Bid, 1), derivativeTestOrder(buyer, Ask, 1)
	reserveDerivativeTestOrder(t, manager, closeBid)
	reserveDerivativeTestOrder(t, manager, closeAsk)
	trade = &Trade{ID: uuid.New(), Symbol: "SIM-FUT", Quantity: 1, Price: 10000, BidOrder: closeBid.ID, AskOrder: closeAsk.ID, BidClient: seller, AskClient: buyer}
	if err := manager.settleTrade(trade); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{buyer, seller} {
		position, _ := manager.GetDerivativePosition(id, "SIM-FUT")
		account, _ := manager.GetMarginAccount(id)
		client, _ := manager.GetClient(id)
		if position != (DerivativePosition{}) || account.AvailableCollateral != 300 || account.PositionMargin != 0 || account.OrderMargin != 0 || client.Exposure != 0 {
			t.Fatalf("closing did not release margin: position=%+v account=%+v client=%+v", position, account, client)
		}
	}
}

func TestDerivativeMarginCannotOffsetDifferentInstruments(t *testing.T) {
	manager, buyer, _ := newDerivativeRiskTest(t, 100, 200)
	if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: "OTHER-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 150}); err != nil {
		t.Fatal(err)
	}
	reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Bid, 1))
	other := derivativeTestOrder(buyer, Ask, 1)
	other.Symbol = "OTHER-FUT"
	if result := manager.checkAndReserveOrder(other); result.Approved || !strings.Contains(result.Reason, ErrInsufficientMargin.Error()) {
		t.Fatalf("cross-instrument collateral reuse: %+v", result)
	}
	if _, exists := manager.GetDerivativePosition(buyer, "OTHER-FUT"); exists {
		t.Fatal("rejected instrument created a position")
	}
}

func TestDerivativePositionAndVolumeLimitsIncludeOpenOrders(t *testing.T) {
	t.Run("position", func(t *testing.T) {
		manager, buyer, _ := newDerivativeRiskTest(t, 100, 1000)
		manager.SetRiskLimits(buyer, &RiskLimits{MaxOrderSize: 100, MaxDailyVolume: 1000, MaxPositionSize: 2})
		reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Ask, 2))
		if result := manager.checkAndReserveOrder(derivativeTestOrder(buyer, Ask, 1)); result.Approved || !strings.Contains(result.Reason, ErrPositionLimitExceeded.Error()) {
			t.Fatalf("position limit ignored pending orders: %+v", result)
		}
	})
	t.Run("volume", func(t *testing.T) {
		manager, buyer, _ := newDerivativeRiskTest(t, 100, 1000)
		manager.SetRiskLimits(buyer, &RiskLimits{MaxOrderSize: 100, MaxDailyVolume: 2, MaxPositionSize: 100})
		reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Bid, 2))
		if result := manager.checkAndReserveOrder(derivativeTestOrder(buyer, Ask, 1)); result.Approved {
			t.Fatal("pending orders bypassed daily volume limit")
		}
	})
}

func TestDerivativeArithmeticBoundsRejectWithoutMutation(t *testing.T) {
	t.Run("marginMultiplication", func(t *testing.T) {
		manager, buyer, _ := newDerivativeRiskTest(t, math.MaxInt64, math.MaxInt64)
		if result := manager.checkAndReserveOrder(derivativeTestOrder(buyer, Bid, 3)); result.Approved {
			t.Fatal("overflowing margin accepted")
		}
		account, _ := manager.GetMarginAccount(buyer)
		if account.OrderMargin != 0 || account.AvailableCollateral != math.MaxInt64 {
			t.Fatal("overflow changed margin")
		}
	})
	t.Run("signedContracts", func(t *testing.T) {
		manager, buyer, _ := newDerivativeRiskTest(t, 1, math.MaxInt64)
		reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Bid, math.MaxInt64))
		if result := manager.checkAndReserveOrder(derivativeTestOrder(buyer, Bid, 1)); result.Approved {
			t.Fatal("overflowing potential position accepted")
		}
	})
}

func TestDerivativeAccountUpdatesAndSnapshotsPreserveReservations(t *testing.T) {
	manager, buyer, _ := newDerivativeRiskTest(t, 100, 200)
	reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Bid, 2))
	if err := manager.AddClient(&Client{ID: buyer, Collateral: 199, MarginCurrency: "SIM"}); !errors.Is(err, ErrInsufficientMargin) {
		t.Fatalf("collateral below reservations returned %v", err)
	}
	if err := manager.AddClient(&Client{ID: buyer, Collateral: 200, MarginCurrency: "OTHER"}); !errors.Is(err, ErrMarginCurrencyMismatch) {
		t.Fatalf("currency change returned %v", err)
	}
	account, _ := manager.GetMarginAccount(buyer)
	account.Collateral = 999
	account.OrderMargin = 0
	position, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	position.OpenBuyContracts = 0
	fresh, _ := manager.GetMarginAccount(buyer)
	freshPosition, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	if fresh.Collateral != 200 || fresh.OrderMargin != 200 || freshPosition.OpenBuyContracts != 2 {
		t.Fatal("financial reads escaped mutable state")
	}
}

func TestInvalidDerivativeTradeDoesNotPartiallySettle(t *testing.T) {
	manager, buyer, seller := newDerivativeRiskTest(t, 100, 300)
	bid, ask := derivativeTestOrder(buyer, Bid, 2), derivativeTestOrder(seller, Ask, 2)
	reserveDerivativeTestOrder(t, manager, bid)
	reserveDerivativeTestOrder(t, manager, ask)
	trade := &Trade{Symbol: "SIM-FUT", Quantity: 3, BidOrder: bid.ID, AskOrder: ask.ID, BidClient: buyer, AskClient: seller}
	if err := manager.settleTrade(trade); !errors.Is(err, ErrInvalidDerivativeTrade) {
		t.Fatalf("invalid trade returned %v", err)
	}
	for _, id := range []uuid.UUID{buyer, seller} {
		position, _ := manager.GetDerivativePosition(id, "SIM-FUT")
		account, _ := manager.GetMarginAccount(id)
		if position.NetContracts != 0 || account.OrderMargin != 200 {
			t.Fatal("invalid trade partially settled")
		}
	}
	if err := manager.UpdatePosition(trade); !errors.Is(err, ErrDerivativeTradeRequiresMatching) {
		t.Fatalf("public position update returned %v", err)
	}
}

func TestDerivativeConfigurationAndClientRequirements(t *testing.T) {
	manager, buyer, _ := newDerivativeRiskTest(t, 100, 300)
	if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 200}); !errors.Is(err, ErrInvalidDerivativeInstrument) {
		t.Fatal("configured margin changed unexpectedly")
	}
	unknown := derivativeTestOrder(uuid.New(), Ask, 1)
	if result := manager.checkAndReserveOrder(unknown); result.Approved {
		t.Fatal("unregistered client accepted")
	}
	otherCurrency := derivativeTestOrder(buyer, Bid, 1)
	otherCurrency.Symbol = "USD-FUT"
	if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: "USD-FUT", MarginCurrency: "USD", InitialMarginPerContract: 100}); err != nil {
		t.Fatal(err)
	}
	if result := manager.checkAndReserveOrder(otherCurrency); result.Approved {
		t.Fatal("incompatible collateral currencies accepted")
	}
	unknown.Symbol = "UNKNOWN"
	if result := manager.checkAndReserveOrder(unknown); result.Approved {
		t.Fatal("unknown instrument accepted")
	}
}

func TestDerivativePortfolioMarginSumCannotOverflow(t *testing.T) {
	manager, buyer, _ := newDerivativeRiskTest(t, math.MaxInt64, math.MaxInt64)
	reserveDerivativeTestOrder(t, manager, derivativeTestOrder(buyer, Bid, 1))
	if err := manager.addDerivativeInstrument(DerivativeInstrument{Symbol: "OTHER-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 1}); err != nil {
		t.Fatal(err)
	}
	other := derivativeTestOrder(buyer, Ask, 1)
	other.Symbol = "OTHER-FUT"
	if result := manager.checkAndReserveOrder(other); result.Approved {
		t.Fatal("portfolio margin sum overflow was accepted")
	}
	account, _ := manager.GetMarginAccount(buyer)
	if account.OrderMargin != math.MaxInt64 || account.AvailableCollateral != 0 {
		t.Fatal("margin sum overflow changed the account")
	}
}

func TestDerivativeRegistrationRequiresCurrencyAndPreservesVolume(t *testing.T) {
	manager, buyer, seller := newDerivativeRiskTest(t, 100, 300)
	if err := manager.AddClient(&Client{ID: uuid.New(), Collateral: 300}); !errors.Is(err, ErrMarginCurrencyMismatch) {
		t.Fatalf("missing collateral currency returned %v", err)
	}
	bid, ask := derivativeTestOrder(buyer, Bid, 1), derivativeTestOrder(seller, Ask, 1)
	reserveDerivativeTestOrder(t, manager, bid)
	reserveDerivativeTestOrder(t, manager, ask)
	if err := manager.settleTrade(&Trade{Symbol: "SIM-FUT", Price: 10000, Quantity: 1, BidOrder: bid.ID, AskOrder: ask.ID, BidClient: buyer, AskClient: seller}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddClient(&Client{ID: buyer, Collateral: 400, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	manager.volumeMu.RLock()
	volume := manager.dailyVolume[buyer]["SIM-FUT"]
	manager.volumeMu.RUnlock()
	if volume != 1 {
		t.Fatal("updating collateral reset daily volume")
	}
}

func TestClosingDerivativeOrdersCannotSpendUnreleasedPositionMargin(t *testing.T) {
	manager, buyer, seller := newDerivativeRiskTest(t, 100, 200)
	bid, ask := derivativeTestOrder(buyer, Bid, 2), derivativeTestOrder(seller, Ask, 2)
	reserveDerivativeTestOrder(t, manager, bid)
	reserveDerivativeTestOrder(t, manager, ask)
	if err := manager.settleTrade(&Trade{Symbol: "SIM-FUT", Price: 10000, Quantity: 2, BidOrder: bid.ID, AskOrder: ask.ID, BidClient: buyer, AskClient: seller}); err != nil {
		t.Fatal(err)
	}
	closing := derivativeTestOrder(buyer, Ask, 2)
	reserveDerivativeTestOrder(t, manager, closing)
	account, _ := manager.GetMarginAccount(buyer)
	if account.PositionMargin != 200 || account.OrderMargin != 0 || account.AvailableCollateral != 0 {
		t.Fatalf("closing order released position margin before a fill: %+v", account)
	}
	if result := manager.checkAndReserveOrder(derivativeTestOrder(buyer, Bid, 1)); result.Approved {
		t.Fatal("opening order spent margin from an unfilled closing order")
	}
	manager.releaseDerivativeOrder(closing.ID, closing.Symbol)
	account, _ = manager.GetMarginAccount(buyer)
	if account.PositionMargin != 200 || account.AvailableCollateral != 0 {
		t.Fatal("cancelling a closing order released position margin")
	}
}

func TestDerivativeTradeCanCloseAndReversePositions(t *testing.T) {
	manager, buyer, seller := newDerivativeRiskTest(t, 100, 200)
	bid, ask := derivativeTestOrder(buyer, Bid, 1), derivativeTestOrder(seller, Ask, 1)
	reserveDerivativeTestOrder(t, manager, bid)
	reserveDerivativeTestOrder(t, manager, ask)
	if err := manager.settleTrade(&Trade{Symbol: "SIM-FUT", Price: 10000, Quantity: 1, BidOrder: bid.ID, AskOrder: ask.ID, BidClient: buyer, AskClient: seller}); err != nil {
		t.Fatal(err)
	}
	reverseBid, reverseAsk := derivativeTestOrder(seller, Bid, 3), derivativeTestOrder(buyer, Ask, 3)
	reserveDerivativeTestOrder(t, manager, reverseBid)
	reserveDerivativeTestOrder(t, manager, reverseAsk)
	if err := manager.settleTrade(&Trade{Symbol: "SIM-FUT", Price: 10000, Quantity: 3, BidOrder: reverseBid.ID, AskOrder: reverseAsk.ID, BidClient: seller, AskClient: buyer}); err != nil {
		t.Fatal(err)
	}
	first, _ := manager.GetDerivativePosition(buyer, "SIM-FUT")
	second, _ := manager.GetDerivativePosition(seller, "SIM-FUT")
	if first.NetContracts != -2 || second.NetContracts != 2 {
		t.Fatalf("positions did not reverse: %+v %+v", first, second)
	}
	for _, id := range []uuid.UUID{buyer, seller} {
		account, _ := manager.GetMarginAccount(id)
		if account.PositionMargin != 200 || account.OrderMargin != 0 || account.AvailableCollateral != 0 {
			t.Fatalf("reversal margin: %+v", account)
		}
	}
}
