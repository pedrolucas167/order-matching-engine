package httpapi

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func newFuturesPnLTest(t *testing.T, collateral, multiplier uint64) (*matching.MatchingEngine, uuid.UUID, uuid.UUID) {
	t.Helper()
	engine := matching.NewDerivativesMatchingEngine(512, 512)
	t.Cleanup(engine.Close)
	if err := engine.AddDerivativeInstrument(matching.DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80, ContractMultiplier: multiplier}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := engine.GetRiskManager().AddClient(&matching.Client{ID: id, Collateral: collateral, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	return engine, buyer, seller
}

func derivativeTestOrder(client uuid.UUID, side matching.Side, quantity uint64) *matching.Order {
	return &matching.Order{ID: uuid.New(), ClientID: client, Symbol: "SIM-FUT", Side: side, Type: matching.Limit, Price: 10000, Quantity: quantity, Remaining: quantity, Status: matching.Pending}
}

func newDurableHTTPFixture(t *testing.T) (*matching.DurableMatchingEngine, string, uuid.UUID, uuid.UUID) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.db")
	engine := reopenDurableHTTPFixture(t, path)
	if err := engine.AddDerivativeInstrument(matching.DerivativeInstrument{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 80}); err != nil {
		t.Fatal(err)
	}
	buyer, seller := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{buyer, seller} {
		if err := engine.AddClient(&matching.Client{ID: id, Collateral: 1000, MarginCurrency: "SIM"}); err != nil {
			t.Fatal(err)
		}
	}
	return engine, path, buyer, seller
}

func reopenDurableHTTPFixture(t *testing.T, path string) *matching.DurableMatchingEngine {
	t.Helper()
	engine, err := matching.NewDurableDerivativesMatchingEngine(path, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

type lostReceiptSubmitter struct {
	engine *matching.MatchingEngine
	lost   atomic.Bool
}

func (submitter *lostReceiptSubmitter) SubmitOrderRequest(ctx context.Context, key string, order *matching.Order) (matching.OrderReceipt, bool, error) {
	receipt, replayed, err := submitter.engine.SubmitOrderRequest(ctx, key, order)
	if err != nil {
		return receipt, replayed, err
	}
	if submitter.lost.CompareAndSwap(false, true) {
		return matching.OrderReceipt{}, false, errors.Join(matching.ErrRequestOutcomeUnknown, context.DeadlineExceeded)
	}
	return receipt, replayed, nil
}
