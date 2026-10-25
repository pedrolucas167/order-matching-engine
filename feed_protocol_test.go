package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPriceFeedRejectsRedirectWithoutFollowingTarget(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	var followed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed.Add(1)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- engine.RunMarkPriceFeed(ctx, MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", BaseURL: server.URL, PriceScale: 1, PollInterval: time.Second, MaxAge: time.Second, RequestTimeout: time.Second})
	}()
	status := waitFeed(t, engine, func(s MarkPriceFeedStatus) bool { return strings.Contains(s.LastError, "302") })
	if status.Fresh || followed.Load() != 0 {
		t.Fatal("redirect followed or accepted as fresh market data")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("redirecting feed did not stop")
	}
}

func TestUnavailableFeedReleasesStopReservation(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	order := derivativeTestOrder(buyer, Bid, 1)
	order.Type = StopMarket
	order.StopPrice = 1000
	submitTestOrder(t, engine, order)
	if engine.GetStats().PendingStopOrders != 1 {
		t.Fatal("stop was not admitted")
	}
	engine.runCommand(func() {
		engine.priceFeeds["SIM-FUT"] = &priceFeedState{config: MarkPriceFeedConfig{Symbol: "SIM-FUT", MaxAge: time.Second}}
		engine.expirePriceFeeds()
	})
	account, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	if engine.GetStats().PendingStopOrders != 0 || account.OrderMargin != 0 || engine.CancelStopOrder(order.ID, "SIM-FUT") {
		t.Fatal("unavailable feed left a pending stop or reservation")
	}
}

func TestPriceProtocolRejectsTransportAndInputFailures(t *testing.T) {
	client := &http.Client{Transport: feedErrorTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport detail") })}
	_, _, err := fetchMarkPrice(context.Background(), client, "https://example.com/private", MarkPriceFeedConfig{})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("transport failed without a sanitized error: %v", err)
	}
	if _, _, err := fetchMarkPrice(context.Background(), client, "://", MarkPriceFeedConfig{}); !errors.Is(err, ErrInvalidPriceFeed) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value string
		scale uint64
	}{{"1", 0}, {strings.Repeat("1", 129), 1}, {"", 1}} {
		if _, err := scaledPrice(tc.value, tc.scale); !errors.Is(err, ErrInvalidPriceFeed) {
			t.Fatal("unsupported scale or decimal length accepted", err)
		}
	}
}
