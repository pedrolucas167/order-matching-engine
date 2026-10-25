package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func waitFeed(t *testing.T, engine *MatchingEngine, predicate func(MarkPriceFeedStatus) bool) MarkPriceFeedStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, ok := engine.GetMarkPriceFeedStatus("SIM-FUT")
		if ok && predicate(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed state timeout: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPriceFeedDisconnectRecoveryAndShutdown(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/premiumIndex" || r.URL.Query().Get("symbol") != "BTCUSDT" {
			t.Error("incorrect public endpoint")
		}
		if failed.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		fmt.Fprintf(w, `{"symbol":"BTCUSDT","markPrice":"100.2500","time":%d}`, time.Now().UnixMilli())
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", BaseURL: server.URL, PriceScale: 100, PollInterval: 5 * time.Millisecond, MaxAge: time.Second, RequestTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- engine.RunMarkPriceFeed(ctx, config) }()
	status := waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return s.Fresh })
	if status.Price != 10025 {
		t.Fatalf("decimal price corrupted: %+v", status)
	}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 100); !errors.Is(err, ErrPriceFeedOwnsMark) {
		t.Fatal("manual feed override accepted", err)
	}
	order := derivativeTestOrder(buyer, Bid, 1)
	order.Price = 1
	submitTestOrder(t, engine, order)
	synchronizeEngine(t, engine)
	failed.Store(true)
	waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return !s.Connected })
	if _, exists := engine.GetOrderBook("SIM-FUT").GetOrder(order.ID); exists {
		t.Fatal("feed failure left an executable order")
	}
	rejected := derivativeTestOrder(buyer, Bid, 1)
	rejected.Price = 1
	submitTestOrder(t, engine, rejected)
	synchronizeEngine(t, engine)
	if _, exists := engine.GetOrderBook("SIM-FUT").GetOrder(rejected.ID); exists {
		t.Fatal("disconnected instrument accepted order")
	}
	failed.Store(false)
	waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return s.Fresh })
	engine.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrEngineClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("feed survived engine shutdown")
	}
}

func TestFeedRejectsStaleFutureAndReorderedUpdates(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	state := &priceFeedState{config: MarkPriceFeedConfig{Symbol: "SIM-FUT", MaxAge: time.Second, MaxFutureSkew: time.Millisecond}}
	engine.runCommand(func() {
		engine.priceFeeds["SIM-FUT"] = state
		stamp := time.Now().Add(-100 * time.Millisecond)
		if err := engine.acceptFeedPrice(state, 100, stamp); err != nil {
			t.Error(err)
		}
		for _, candidate := range []time.Time{stamp.Add(-time.Millisecond), time.Now().Add(-time.Minute), time.Now().Add(time.Second)} {
			if err := engine.acceptFeedPrice(state, 200, candidate); err == nil {
				t.Error("invalid timestamp accepted")
			}
		}
		if err := engine.acceptFeedPrice(state, 101, stamp); err == nil {
			t.Error("conflicting duplicate accepted")
		}
		if err := engine.acceptFeedPrice(state, 100, stamp); err != nil {
			t.Error("identical duplicate rejected", err)
		}
		if state.status.Price != 100 || !state.status.SourceTime.Equal(stamp) {
			t.Error("invalid update changed mark")
		}
		state.status.SourceTime = time.Now().Add(-time.Minute)
		engine.expirePriceFeeds()
		if state.status.Fresh {
			t.Error("duplicate receipt extended source freshness")
		}
	})
}

func TestScaledPriceRejectsRoundingAndOverflow(t *testing.T) {
	for _, value := range []string{"0", "-1", "+1", "1e2", " 1", "1.001", "18446744073709551616", "1/2", ".1", "1."} {
		if _, err := scaledPrice(value, 100); err == nil {
			t.Errorf("invalid decimal accepted: %s", value)
		}
	}
	price, err := scaledPrice("123.450000", 100)
	if err != nil || price != 12345 {
		t.Fatalf("exact decimal rejected: %d %v", price, err)
	}
	price, err = scaledPrice("010", 1)
	if err != nil || price != 10 {
		t.Fatalf("decimal parsed as another base: %d %v", price, err)
	}
}

func TestFeedCancellationRestartPreservesTimestamp(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	stamp := time.Now().UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"symbol":"BTCUSDT","markPrice":"100","time":%d}`, stamp)
	}))
	defer server.Close()
	config := MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", BaseURL: server.URL, PriceScale: 1, PollInterval: 5 * time.Millisecond, MaxAge: time.Second, RequestTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.RunMarkPriceFeed(ctx, config) }()
	waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return s.Fresh })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("feed did not cancel")
	}
	if err := engine.RunMarkPriceFeed(context.Background(), MarkPriceFeedConfig{}); !errors.Is(err, ErrInvalidPriceFeed) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- engine.RunMarkPriceFeed(ctx, config) }()
	status := waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return s.Fresh })
	if status.SourceTime.UnixMilli() != stamp {
		t.Fatal("restart lost source timestamp")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("restarted feed did not cancel")
	}
}

func TestFeedShutdownCancelsInFlightRequest(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	entered, exited := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- engine.RunMarkPriceFeed(context.Background(), MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", BaseURL: server.URL, PriceScale: 1, PollInterval: time.Second, MaxAge: time.Second, RequestTimeout: time.Minute})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	engine.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request ignored engine shutdown")
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("server request context not canceled")
	}
}

func TestFeedRejectsMalformedResponse(t *testing.T) {
	for _, body := range []string{`{"symbol":"OTHER","markPrice":"100","time":1}`, `{"symbol":"BTCUSDT","markPrice":"100","time":0}`, `{"symbol":"BTCUSDT","markPrice":"NaN","time":1}`, `[]`, `{}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, _, err := fetchMarkPrice(context.Background(), server.Client(), server.URL, MarkPriceFeedConfig{SourceSymbol: "BTCUSDT", PriceScale: 1})
		server.Close()
		if err == nil {
			t.Fatalf("malformed response accepted: %s", body)
		}
	}
}

func TestFeedFailureBlocksRiskAcrossAffectedPortfolio(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "ALT", MarginCurrency: "SIM", InitialMarginPerContract: 100}); err != nil {
		t.Fatal(err)
	}
	order := derivativeTestOrder(buyer, Bid, 1)
	order.Symbol = "ALT"
	order.Price = 1
	submitTestOrder(t, engine, order)
	synchronizeEngine(t, engine)
	engine.runCommand(func() {
		engine.priceFeeds["SIM-FUT"] = &priceFeedState{config: MarkPriceFeedConfig{Symbol: "SIM-FUT", MaxAge: time.Second}}
		engine.expirePriceFeeds()
	})
	if _, exists := engine.GetOrderBook("ALT").GetOrder(order.ID); exists {
		t.Fatal("stale portfolio equity kept pending risk on another instrument")
	}
	rejected := derivativeTestOrder(buyer, Bid, 1)
	rejected.Symbol = "ALT"
	rejected.Price = 1
	submitTestOrder(t, engine, rejected)
	synchronizeEngine(t, engine)
	if _, exists := engine.GetOrderBook("ALT").GetOrder(rejected.ID); exists {
		t.Fatal("stale portfolio equity admitted risk on another instrument")
	}
	engine.runCommand(func() {
		if err := engine.acceptFeedPrice(engine.priceFeeds["SIM-FUT"], 100, time.Now()); err != nil {
			t.Error(err)
		}
	})
	accepted := derivativeTestOrder(buyer, Bid, 1)
	accepted.Symbol = "ALT"
	accepted.Price = 1
	submitTestOrder(t, engine, accepted)
	synchronizeEngine(t, engine)
	if _, exists := engine.GetOrderBook("ALT").GetOrder(accepted.ID); !exists {
		t.Fatal("fresh portfolio did not resume admission")
	}
}
