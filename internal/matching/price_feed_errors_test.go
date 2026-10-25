package matching

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFeedConfigurationErrorsDoNotBindInstrument(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	valid := MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", PriceScale: 100, PollInterval: time.Second, MaxAge: time.Second, RequestTimeout: time.Second}
	mutations := []func(*MarkPriceFeedConfig){
		func(c *MarkPriceFeedConfig) { c.BaseURL = "://" }, func(c *MarkPriceFeedConfig) { c.BaseURL = "ftp://localhost" },
		func(c *MarkPriceFeedConfig) { c.BaseURL = "http://example.com" }, func(c *MarkPriceFeedConfig) { c.BaseURL = "https://user:secret@example.com" },
		func(c *MarkPriceFeedConfig) { c.BaseURL = "https://example.com?query=1" }, func(c *MarkPriceFeedConfig) { c.BaseURL = "https://example.com#fragment" },
		func(c *MarkPriceFeedConfig) { c.Symbol = "" }, func(c *MarkPriceFeedConfig) { c.SourceSymbol = "" }, func(c *MarkPriceFeedConfig) { c.PriceScale = 0 },
		func(c *MarkPriceFeedConfig) { c.PollInterval = time.Millisecond }, func(c *MarkPriceFeedConfig) { c.MaxAge = 0 }, func(c *MarkPriceFeedConfig) { c.MaxFutureSkew = -1 },
		func(c *MarkPriceFeedConfig) { c.MaxFutureSkew = 2 * time.Second }, func(c *MarkPriceFeedConfig) { c.RequestTimeout = 0 },
	}
	for i, mutate := range mutations {
		config := valid
		mutate(&config)
		if err := engine.RunMarkPriceFeed(context.Background(), config); !errors.Is(err, ErrInvalidPriceFeed) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if err := engine.RunMarkPriceFeed(nil, valid); !errors.Is(err, ErrInvalidPriceFeed) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.RunMarkPriceFeed(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	unknown := valid
	unknown.Symbol = "UNKNOWN"
	if err := engine.RunMarkPriceFeed(context.Background(), unknown); !errors.Is(err, ErrUnknownDerivativeInstrument) {
		t.Fatal(err)
	}
	if _, exists := engine.GetMarkPriceFeedStatus("SIM-FUT"); exists {
		t.Fatal("invalid configuration left feed ownership behind")
	}
	engine.Close()
	if err := engine.RunMarkPriceFeed(context.Background(), valid); !errors.Is(err, ErrEngineClosed) {
		t.Fatal(err)
	}
}

func TestFetchPricePreservesCancellationAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := fetchMarkPrice(canceled, server.Client(), server.URL, MarkPriceFeedConfig{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	_, _, err = fetchMarkPrice(context.Background(), client, server.URL, MarkPriceFeedConfig{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request timeout cause lost: %v", err)
	}
}

func TestFetchPricePreservesTimeoutDuringBodyRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	if _, _, err := fetchMarkPrice(context.Background(), client, server.URL, MarkPriceFeedConfig{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body timeout cause lost: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client.Timeout = time.Second
	if _, _, err := fetchMarkPrice(ctx, client, server.URL, MarkPriceFeedConfig{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context timeout cause lost during body read: %v", err)
	}
}

type feedErrorTransport func(*http.Request) (*http.Response, error)

func (transport feedErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type failingPriceBody struct{ closed bool }

func (*failingPriceBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (body *failingPriceBody) Close() error        { body.closed = true; return nil }

func TestFetchPriceClosesBodyOnReadFailure(t *testing.T) {
	body := &failingPriceBody{}
	client := &http.Client{Transport: feedErrorTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	if _, _, err := fetchMarkPrice(context.Background(), client, "https://example.com", MarkPriceFeedConfig{}); !errors.Is(err, ErrInvalidPriceFeed) {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("failed read leaked response body")
	}
}

func TestFetchPriceRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat(" ", 16385)) }))
	defer server.Close()
	if _, _, err := fetchMarkPrice(context.Background(), server.Client(), server.URL, MarkPriceFeedConfig{}); !errors.Is(err, ErrInvalidPriceFeed) {
		t.Fatal(err)
	}
}

func TestFeedValuationOverflowPreservesReferenceAndBalances(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	state := &priceFeedState{config: MarkPriceFeedConfig{Symbol: "SIM-FUT", MaxAge: time.Second}}
	engine.runCommand(func() {
		if err := engine.acceptFeedPrice(state, 110, time.Now().Add(-time.Millisecond)); err != nil {
			t.Error(err)
		}
	})
	before, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	prior := state.status
	engine.runCommand(func() {
		if err := engine.acceptFeedPrice(state, math.MaxUint64, time.Now()); !errors.Is(err, ErrRiskArithmeticOverflow) {
			t.Error(err)
		}
	})
	after, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	mark, _ := engine.GetRiskManager().GetDerivativeMarkPrice("SIM-FUT")
	if after != before || state.status != prior || mark != 110 {
		t.Fatal("rejected feed update partially changed state")
	}
}

func TestFeedOwnershipRejectsConflictingCommands(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	config := MarkPriceFeedConfig{Symbol: "SIM-FUT", SourceSymbol: "BTCUSDT", BaseURL: "https://fapi.binance.com", PriceScale: 1, PollInterval: time.Second, MaxAge: time.Second, RequestTimeout: time.Second}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 100); err != nil {
		t.Fatal(err)
	}
	engine.runCommand(func() { engine.priceFeeds["SIM-FUT"] = &priceFeedState{config: config, running: true} })
	if err := engine.RunMarkPriceFeed(context.Background(), config); !errors.Is(err, ErrPriceFeedOwnsMark) {
		t.Fatal(err)
	}
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 200); !errors.Is(err, ErrPriceFeedOwnsMark) {
		t.Fatal(err)
	}
	engine.runCommand(func() { engine.priceFeeds["SIM-FUT"].running = false })
	config.PriceScale = 100
	if err := engine.RunMarkPriceFeed(context.Background(), config); !errors.Is(err, ErrPriceFeedOwnsMark) {
		t.Fatal(err)
	}
	mark, _ := engine.GetRiskManager().GetDerivativeMarkPrice("SIM-FUT")
	if mark != 100 {
		t.Fatal("conflicting command replaced authoritative mark")
	}
}
