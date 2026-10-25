package matching

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestDurableCheckpointExhaustionPreservesLastCommittedAccount(t *testing.T) {
	engine, path, buyer, _ := newDurableFuturesTest(t)
	if err := engine.mutate(func() error {
		engine.checkpoint = math.MaxUint64 - 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client := &Client{ID: uuid.New(), Collateral: 500, MarginCurrency: "SIM"}
	if err := engine.AddClient(client); !errors.Is(err, ErrDurableStorage) || !errors.Is(err, ErrInvalidDurableState) {
		t.Fatal("checkpoint exhaustion must fence the engine", err)
	}
	if _, err := engine.GetClient(buyer); !errors.Is(err, ErrDurableStorage) {
		t.Fatal("fenced engine exposed uncertain state", err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	if _, err := recovered.GetClient(client.ID); !errors.Is(err, ErrClientNotFound) {
		t.Fatal("uncommitted account survived recovery", err)
	}
	if account, err := recovered.GetClient(buyer); err != nil || account.Collateral != 1000 {
		t.Fatal("committed funding changed", account, err)
	}
}

func TestDurableTradeSequenceExhaustionDoesNotConfirmExecution(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	ask := derivativeTestOrder(seller, Ask, 1)
	ask.Price = 100
	durableSubmit(t, engine, "maker", ask)
	if err := engine.mutate(func() error {
		engine.tradeSequence = math.MaxUint64
		engine.acknowledgedTrade = math.MaxUint64
		engine.engine.tradesProcessed.Store(math.MaxUint64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Price = 100
	receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "taker", bid)
	if !errors.Is(err, ErrRequestOutcomeUnknown) || !errors.Is(err, ErrDurableStorage) || replayed || receipt.OrderID != uuid.Nil {
		t.Fatal("exhausted trade sequence returned a successful receipt", receipt, err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	position, _, err := recovered.GetDerivativePosition(buyer, "SIM-FUT")
	if err != nil || position.NetContracts != 0 {
		t.Fatal("uncommitted execution changed recovered position", position, err)
	}
	snapshot, err := recovered.GetSnapshot("SIM-FUT")
	if err != nil || snapshot == nil || snapshot.BestAsk == nil || snapshot.BestAsk.TotalQty != 1 {
		t.Fatal("uncommitted execution consumed the maker", snapshot, err)
	}
}

func TestDurableEncodingFailureDoesNotPersistInvalidClient(t *testing.T) {
	engine, path, buyer, _ := newDurableFuturesTest(t)
	client := &Client{ID: uuid.New(), Name: string([]byte{0xff}), Collateral: 500, MarginCurrency: "SIM"}
	if err := engine.AddClient(client); !errors.Is(err, ErrDurableStorage) {
		t.Fatal("invalid UTF-8 must not produce a durable acknowledgment", err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	if _, err := recovered.GetClient(client.ID); !errors.Is(err, ErrClientNotFound) {
		t.Fatal("unencodable client survived recovery", err)
	}
	if account, err := recovered.GetClient(buyer); err != nil || account.Collateral != 1000 {
		t.Fatal("encoding failure changed committed funding", account, err)
	}
}

func TestDurablePartialAcknowledgmentRetainsUnprocessedTrades(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	ask := derivativeTestOrder(seller, Ask, 2)
	ask.Price = 100
	durableSubmit(t, engine, "maker", ask)
	for _, key := range []string{"first", "second"} {
		bid := derivativeTestOrder(buyer, Bid, 1)
		bid.Price = 100
		durableSubmit(t, engine, key, bid)
	}
	before, err := engine.ReadTrades(0, 2)
	if err != nil || len(before) != 2 {
		t.Fatal(before, err)
	}
	if err := engine.AcknowledgeTrades(1); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	after, err := recovered.ReadTrades(1, 2)
	if err != nil || len(after) != 1 || after[0] != before[1] {
		t.Fatal("partial acknowledgment lost or changed the remaining trade", after, err)
	}
	state, err := recovered.GetTradeDeliveryState()
	if err != nil || state.Acknowledged != 1 || state.Latest != 2 || state.Pending != 1 {
		t.Fatal("recovered acknowledgment cursor is inconsistent", state, err)
	}
}

func TestDurableLiquidationRecoveryKeepsAccountsAndSymbolsSeparate(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "ALT", MarginCurrency: "SIM", InitialMarginPerContract: 100}); err != nil {
		t.Fatal(err)
	}
	statuses := map[liquidationKey]LiquidationStatus{
		{buyer, "SIM-FUT"}:  {AttemptedQuantity: 2, RemainingQuantity: 2, Reason: "no_liquidity"},
		{buyer, "ALT"}:      {AttemptedQuantity: 3, RemainingQuantity: 3, Reason: "no_liquidity"},
		{seller, "SIM-FUT"}: {AttemptedQuantity: 4, RemainingQuantity: 4, Reason: "no_liquidity"},
	}
	if err := engine.mutate(func() error {
		for key, status := range statuses {
			engine.engine.liquidations[key] = status
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	for key, expected := range statuses {
		actual, exists := recovered.engine.GetLiquidationStatus(key.ClientID, key.Symbol)
		if !exists || actual != expected {
			t.Fatal("liquidation recovery mixed accounts or instruments", key, actual, expected)
		}
	}
}
