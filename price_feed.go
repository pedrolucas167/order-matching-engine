package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidPriceFeed  = errors.New("invalid price feed configuration or update")
	ErrPriceFeedOwnsMark = errors.New("external price feed owns the instrument mark")
)

type MarkPriceFeedConfig struct {
	Symbol         string
	SourceSymbol   string
	BaseURL        string
	PriceScale     uint64
	PollInterval   time.Duration
	MaxAge         time.Duration
	MaxFutureSkew  time.Duration
	RequestTimeout time.Duration
}

type MarkPriceFeedStatus struct {
	Connected  bool
	Fresh      bool
	Price      uint64
	SourceTime time.Time
	LastError  string
}

type priceFeedState struct {
	config  MarkPriceFeedConfig
	status  MarkPriceFeedStatus
	running bool
}

// RunMarkPriceFeed blocks until cancellation or engine shutdown; only public market data is requested.
func (me *MatchingEngine) RunMarkPriceFeed(ctx context.Context, config MarkPriceFeedConfig) error {
	if ctx == nil {
		return ErrInvalidPriceFeed
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://fapi.binance.com"
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && base.Scheme != "http") ||
		config.Symbol == "" || config.SourceSymbol == "" || config.PriceScale == 0 || config.PollInterval <= 0 || config.MaxAge <= 0 || config.MaxFutureSkew < 0 || config.MaxFutureSkew > config.MaxAge || config.RequestTimeout <= 0 {
		return ErrInvalidPriceFeed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if base.Scheme == "http" && base.Hostname() != "localhost" && !net.ParseIP(base.Hostname()).IsLoopback() {
		return ErrInvalidPriceFeed
	}
	if base.Hostname() != "localhost" && !net.ParseIP(base.Hostname()).IsLoopback() && config.PollInterval < time.Second {
		return ErrInvalidPriceFeed
	}
	state := &priceFeedState{config: config, running: true}
	var bindError error
	if !me.runCommand(func() {
		me.riskManager.accountMu.RLock()
		_, known := me.riskManager.derivativeInstruments[config.Symbol]
		me.riskManager.accountMu.RUnlock()
		if !known {
			bindError = ErrUnknownDerivativeInstrument
			return
		}
		if previous := me.priceFeeds[config.Symbol]; previous != nil {
			if previous.running || previous.config.SourceSymbol != config.SourceSymbol || previous.config.PriceScale != config.PriceScale || previous.config.BaseURL != config.BaseURL {
				bindError = ErrPriceFeedOwnsMark
				return
			}
			state.status.Price = previous.status.Price
			state.status.SourceTime = previous.status.SourceTime
		}
		me.priceFeeds[config.Symbol] = state
		me.expirePriceFeeds()
	}) {
		return ErrEngineClosed
	}
	if bindError != nil {
		return bindError
	}
	feedContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-me.stop:
			cancel()
		case <-stopped:
		}
	}()
	defer me.runCommand(func() {
		state.running = false
		state.status.Connected = false
		state.status.Fresh = false
		state.status.LastError = "feed stopped"
		me.expirePriceFeeds()
	})
	base.Path = strings.TrimRight(base.Path, "/") + "/fapi/v1/premiumIndex"
	query := base.Query()
	query.Set("symbol", config.SourceSymbol)
	base.RawQuery = query.Encode()
	client := &http.Client{Timeout: config.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()
	for {
		price, stamp, fetchError := fetchMarkPrice(feedContext, client, base.String(), config)
		if !me.runCommand(func() {
			updateError := fetchError
			if updateError == nil {
				updateError = me.acceptFeedPrice(state, price, stamp)
			}
			if updateError != nil {
				state.status.Connected = false
				state.status.Fresh = false
				state.status.LastError = updateError.Error()
				me.expirePriceFeeds()
			}
		}) {
			return ErrEngineClosed
		}
		select {
		case <-me.stop:
			return ErrEngineClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func fetchMarkPrice(ctx context.Context, client *http.Client, endpoint string, config MarkPriceFeedConfig) (uint64, time.Time, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, time.Time{}, ErrInvalidPriceFeed
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, time.Time{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, time.Time{}, context.DeadlineExceeded
		}
		return 0, time.Time{}, errors.New("price request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, time.Time{}, fmt.Errorf("price response status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil {
		if ctx.Err() != nil {
			return 0, time.Time{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, time.Time{}, context.DeadlineExceeded
		}
		return 0, time.Time{}, ErrInvalidPriceFeed
	}
	if len(body) > 16384 {
		return 0, time.Time{}, ErrInvalidPriceFeed
	}
	var payload struct {
		Symbol    string `json:"symbol"`
		MarkPrice string `json:"markPrice"`
		Time      int64  `json:"time"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Symbol != config.SourceSymbol || payload.Time <= 0 {
		return 0, time.Time{}, ErrInvalidPriceFeed
	}
	price, err := scaledPrice(payload.MarkPrice, config.PriceScale)
	return price, time.UnixMilli(payload.Time), err
}

func scaledPrice(value string, scale uint64) (uint64, error) {
	if value == "" || len(value) > 128 || scale == 0 {
		return 0, ErrInvalidPriceFeed
	}
	dots := 0
	for _, c := range value {
		if c == '.' {
			dots++
		} else if c < '0' || c > '9' {
			return 0, ErrInvalidPriceFeed
		}
	}
	if dots > 1 || value[0] == '.' || value[len(value)-1] == '.' {
		return 0, ErrInvalidPriceFeed
	}
	digits := strings.ReplaceAll(value, ".", "")
	number, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, ErrInvalidPriceFeed
	}
	number.Mul(number, new(big.Int).SetUint64(scale))
	decimals := 0
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		decimals = len(value) - dot - 1
	}
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	remainder := new(big.Int)
	number.QuoRem(number, divisor, remainder)
	if remainder.Sign() != 0 || number.Sign() <= 0 || !number.IsUint64() {
		return 0, ErrInvalidPriceFeed
	}
	return number.Uint64(), nil
}

func (me *MatchingEngine) acceptFeedPrice(state *priceFeedState, price uint64, stamp time.Time) error {
	now := time.Now()
	if price == 0 || now.Sub(stamp) > state.config.MaxAge || stamp.After(now.Add(state.config.MaxFutureSkew)) || stamp.Before(state.status.SourceTime) {
		return ErrInvalidPriceFeed
	}
	if stamp.Equal(state.status.SourceTime) {
		if price != state.status.Price {
			return ErrInvalidPriceFeed
		}
	} else {
		if err := me.riskManager.setDerivativeMarkPrice(state.config.Symbol, price); err != nil {
			return err
		}
		state.status.Price = price
		state.status.SourceTime = stamp
		me.markTimes[state.config.Symbol] = stamp
	}
	state.status.Connected = true
	state.status.Fresh = true
	state.status.LastError = ""
	me.cancelUndermarginedOrders()
	return nil
}

func (me *MatchingEngine) GetMarkPriceFeedStatus(symbol string) (MarkPriceFeedStatus, bool) {
	var status MarkPriceFeedStatus
	var exists bool
	me.runCommand(func() {
		if state := me.priceFeeds[symbol]; state != nil {
			status = state.status
			exists = true
		}
	})
	return status, exists
}

func (me *MatchingEngine) priceFeedReady(symbol string) bool {
	state := me.priceFeeds[symbol]
	return state == nil || (state.status.Connected && state.status.Fresh && time.Since(state.status.SourceTime) <= state.config.MaxAge)
}

func (me *MatchingEngine) expirePriceFeeds() {
	if len(me.priceFeeds) == 0 {
		return
	}
	unavailable := make(map[string]bool)
	for symbol, state := range me.priceFeeds {
		if !me.priceFeedReady(symbol) {
			state.status.Fresh = false
			unavailable[symbol] = true
			me.cancelSymbolOrders(symbol)
		}
	}
	if len(unavailable) == 0 {
		return
	}
	accounts := make(map[uuid.UUID]bool)
	me.riskManager.accountMu.RLock()
	for clientID, positions := range me.riskManager.derivativePositions {
		for symbol, position := range positions {
			if unavailable[symbol] && position.NetContracts != 0 {
				accounts[clientID] = true
				break
			}
		}
	}
	me.riskManager.accountMu.RUnlock()
	me.cancelAccountOrders(accounts)
}

func (me *MatchingEngine) accountPriceFeedsReady(clientID uuid.UUID) bool {
	if len(me.priceFeeds) == 0 {
		return true
	}
	me.riskManager.accountMu.RLock()
	defer me.riskManager.accountMu.RUnlock()
	for symbol, position := range me.riskManager.derivativePositions[clientID] {
		if position.NetContracts != 0 && !me.priceFeedReady(symbol) {
			return false
		}
	}
	return true
}

func (me *MatchingEngine) cancelSymbolOrders(symbol string) {
	if book := me.books[symbol]; book != nil {
		for id, order := range book.orders {
			order.Status = Cancelled
			me.riskManager.releaseDerivativeOrder(id, symbol)
			book.removeOrder(id)
		}
	}
	for _, order := range me.stopOrders[symbol] {
		order.Status = Cancelled
		me.riskManager.releaseDerivativeOrder(order.ID, symbol)
	}
	delete(me.stopOrders, symbol)
}
