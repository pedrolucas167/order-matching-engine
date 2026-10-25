package matching

import (
	"context"
	"errors"

	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDurableClientReadDoesNotExposeMutableFunding(t *testing.T) {
	engine, _, buyer, _ := newDurableFuturesTest(t)
	client, err := engine.GetClient(buyer)
	if err != nil || client.Collateral != 1000 {
		t.Fatal(err)
	}
	client.Collateral = 0
	unchanged, err := engine.GetClient(buyer)
	if err != nil || unchanged.Collateral != 1000 {
		t.Fatal("client query exposed mutable funding", err)
	}
	if _, err := engine.GetClient(uuid.New()); !errors.Is(err, ErrClientNotFound) {
		t.Fatal(err)
	}
	engine.Close()
	if _, err := engine.GetClient(buyer); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
}

func TestDurableOutboxBackpressurePagesAndAcknowledgments(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	engine.Close()
	engine, err := NewDurableDerivativesMatchingEngine(path, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ask := derivativeTestOrder(seller, Ask, 2)
	ask.Price = 100
	durableSubmit(t, engine, "maker", ask)
	bid := derivativeTestOrder(buyer, Bid, 1)
	bid.Price = 100
	durableSubmit(t, engine, "first", bid)
	next := derivativeTestOrder(buyer, Bid, 1)
	next.Price = 100
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "next", next); !errors.Is(err, ErrBackpressure) {
		t.Fatal("unacknowledged outbox did not apply backpressure", err)
	}
	if _, replayed, err := engine.SubmitOrderRequest(context.Background(), "first", bid); err != nil || !replayed {
		t.Fatal("outbox pressure blocked replay", err)
	}
	for _, tc := range []struct {
		after uint64
		limit int
	}{{0, 0}, {0, 10001}, {2, 1}} {
		if _, err := engine.ReadTrades(tc.after, tc.limit); err == nil {
			t.Fatal("invalid delivery cursor accepted")
		}
	}
	if err := engine.AcknowledgeTrades(2); err == nil {
		t.Fatal("unknown trade acknowledged")
	}
	events, err := engine.ReadTrades(0, 1)
	if err != nil || len(events) != 1 {
		t.Fatal(err)
	}
	events[0].Trade.Price = 999
	again, _ := engine.ReadTrades(0, 1)
	if again[0].Trade.Price != 100 {
		t.Fatal("delivery query leaked mutable state")
	}
	if err := engine.AcknowledgeTrades(1); err != nil {
		t.Fatal(err)
	}
	durableSubmit(t, engine, "next", next)
	state, err := engine.GetTradeDeliveryState()
	if err != nil || state.Acknowledged != 1 || state.Latest != 2 || state.Pending != 1 {
		t.Fatalf("unexpected delivery state: %+v %v", state, err)
	}
	if err := engine.AcknowledgeTrades(2); err != nil {
		t.Fatal(err)
	}
	if events, err := engine.ReadTrades(2, 1); err != nil || len(events) != 0 {
		t.Fatal(err)
	}
}

func TestDurableConfigurationValuationAndClosedAPIs(t *testing.T) {
	engine, path, buyer, seller := newDurableFuturesTest(t)
	if err := engine.SetRiskLimits(buyer, &RiskLimits{MaxOrderSize: 10, MaxDailyVolume: 10, MaxPositionSize: 10}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddCircuitBreaker(&CircuitBreaker{Symbol: "OTHER", ThresholdPct: 20}); err != nil {
		t.Fatal(err)
	}
	ask, bid := derivativeTestOrder(seller, Ask, 1), derivativeTestOrder(buyer, Bid, 1)
	ask.Price = 100
	bid.Price = 100
	durableSubmit(t, engine, "ask", ask)
	durableSubmit(t, engine, "bid", bid)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 110); err != nil {
		t.Fatal(err)
	}
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 110); err != nil {
		t.Fatal(err)
	}
	if err := engine.ResetDailyVolume(buyer); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetOrderRequestCapacity(2); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetOrderRequestCapacity(1); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal(err)
	}
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetLiquidationPolicy(LiquidationPolicy{MaxMarkAge: -1}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if err := engine.SetOrderRequestCapacity(0); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal(err)
	}
	if err := engine.AddClient(nil); !errors.Is(err, ErrClientNotFound) {
		t.Fatal(err)
	}
	if err := engine.SetRiskLimits(buyer, nil); !errors.Is(err, ErrInvalidRiskLimits) {
		t.Fatal(err)
	}
	if err := engine.AddCircuitBreaker(nil); !errors.Is(err, ErrInvalidCircuitBreaker) {
		t.Fatal(err)
	}
	if err := engine.SetDerivativeMarkPrice("UNKNOWN", 1); !errors.Is(err, ErrUnknownDerivativeInstrument) {
		t.Fatal(err)
	}
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 0); !errors.Is(err, ErrUnknownMarkPrice) {
		t.Fatal(err)
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	account, err := recovered.GetMarginAccount(buyer)
	if err != nil || account.RealizedPnL != 10 || account.UnrealizedPnL != 0 || account.Equity != 1010 {
		t.Fatalf("settlement did not survive: %+v %v", account, err)
	}
	if _, _, err := recovered.SubmitOrderRequest(context.Background(), "new", derivativeTestOrder(buyer, Bid, 1)); !errors.Is(err, ErrRequestCapacity) {
		t.Fatal("request capacity not restored", err)
	}
	recovered.Close()
	if _, err := recovered.GetTradeDeliveryState(); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if _, err := recovered.ReadTrades(0, 1); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if _, err := recovered.GetMarginAccount(buyer); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if _, _, err := recovered.GetDerivativePosition(buyer, "SIM-FUT"); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if _, err := recovered.GetSnapshot("SIM-FUT"); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if _, err := recovered.GetStats(); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableConcurrentReplayWritesOnlyOneReceipt(t *testing.T) {
	engine, path, buyer, _ := newDurableFuturesTest(t)
	var writes atomic.Int32
	engine.mu.Lock()
	write := engine.writeState
	engine.writeState = func(payload []byte) error { writes.Add(1); return write(payload) }
	engine.mu.Unlock()
	order := derivativeTestOrder(buyer, Bid, 1)
	results := make(chan orderRequestResult, 32)
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			receipt, replayed, err := engine.SubmitOrderRequest(context.Background(), "same", order)
			results <- orderRequestResult{receipt, replayed, err}
		}()
	}
	group.Wait()
	close(results)
	original := 0
	for result := range results {
		if result.err != nil || result.receipt.Sequence != 1 {
			t.Fatal(result.err)
		}
		if !result.replayed {
			original++
		}
	}
	if original != 1 || writes.Load() != 1 {
		t.Fatalf("replay caused duplicate persistence: original=%d writes=%d", original, writes.Load())
	}
	engine.Close()
	recovered := reopenDurableTest(t, path)
	if _, replayed, err := recovered.SubmitOrderRequest(context.Background(), "same", order); err != nil || !replayed {
		t.Fatal(err)
	}
}

func TestDurableAdmissionBoundsSlowWritesAndHidesCandidateState(t *testing.T) {
	initial, path, buyer, _ := newDurableFuturesTest(t)
	initial.Close()
	engine, err := NewDurableDerivativesMatchingEngine(path, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	engine.mu.Lock()
	write := engine.writeState
	engine.writeState = func(payload []byte) error { close(entered); <-release; return write(payload) }
	engine.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.SubmitOrderRequest(context.Background(), "blocked", derivativeTestOrder(buyer, Bid, 1))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("write did not block")
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "overflow", derivativeTestOrder(buyer, Bid, 1)); !errors.Is(err, ErrBackpressure) {
		t.Fatal("unbounded admission during slow commit", err)
	}
	visible := make(chan error, 1)
	go func() { _, err := engine.GetMarginAccount(buyer); visible <- err }()
	select {
	case err := <-visible:
		t.Fatalf("candidate state leaked before commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-visible; err != nil {
		t.Fatal(err)
	}
}

func TestDurableInputValidation(t *testing.T) {
	for _, tc := range []struct {
		path           string
		orders, trades int
	}{{"", 1, 1}, {filepath.Join(t.TempDir(), "x"), 0, 1}, {filepath.Join(t.TempDir(), "x"), 1, 0}} {
		if engine, err := NewDurableDerivativesMatchingEngine(tc.path, tc.orders, tc.trades); err == nil {
			engine.Close()
			t.Fatal("invalid durable engine configuration accepted")
		}
	}
	engine, _, buyer, _ := newDurableFuturesTest(t)
	order := derivativeTestOrder(buyer, Bid, 1)
	if _, _, err := engine.SubmitOrderRequest(nil, "key", order); !errors.Is(err, ErrInvalidOrderRequest) {
		t.Fatal(err)
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "", order); !errors.Is(err, ErrInvalidOrderRequest) {
		t.Fatal(err)
	}
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "key", nil); !errors.Is(err, ErrInvalidOrder) {
		t.Fatal(err)
	}
	invalid := *order
	invalid.ID = uuid.Nil
	if _, _, err := engine.SubmitOrderRequest(context.Background(), "key", &invalid); !errors.Is(err, ErrInvalidOrder) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := engine.SubmitOrderRequest(ctx, "key", order); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
