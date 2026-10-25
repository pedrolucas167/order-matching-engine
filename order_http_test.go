package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func httpOrderBody(t *testing.T, order *Order) string {
	t.Helper()
	side := "buy"
	if order.Side == Ask {
		side = "sell"
	}
	types := [...]string{"limit", "market", "stop_market", "stop_limit"}
	encoded, err := json.Marshal(HTTPOrderRequest{OrderID: order.ID, ClientID: order.ClientID, Symbol: order.Symbol, Side: side, Type: types[order.Type], Quantity: order.Quantity, Price: order.Price, StopPrice: order.StopPrice})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func orderHTTPCall(handler http.Handler, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestHTTPOrderReplayReturnsIdenticalReceipt(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	handler, err := NewOrderHTTPHandler(engine)
	if err != nil {
		t.Fatal(err)
	}
	body := httpOrderBody(t, derivativeTestOrder(buyer, Bid, 1))
	first := orderHTTPCall(handler, body, "order")
	replay := orderHTTPCall(handler, body, "order")
	if first.Code != 200 || replay.Code != 200 || first.Body.String() != replay.Body.String() || first.Header().Get("Idempotency-Replayed") != "false" || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("HTTP receipt is not replayable: %d %s / %d %s", first.Code, first.Body, replay.Code, replay.Body)
	}
	conflict := orderHTTPCall(handler, strings.Replace(body, `"quantity":"1"`, `"quantity":"2"`, 1), "order")
	if conflict.Code != 409 {
		t.Fatalf("conflict returned %d: %s", conflict.Code, conflict.Body)
	}
}

func TestHTTPOrderRejectsAmbiguousAndInvalidPayloads(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	handler, _ := NewOrderHTTPHandler(engine)
	valid := httpOrderBody(t, derivativeTestOrder(buyer, Bid, 1))
	cases := []string{`null`, `[]`, `{}`, valid + valid, strings.Replace(valid, `"symbol":`, `"SYMBOL":`, 1), strings.Replace(valid, `"symbol":`, `"symbol":"OTHER","symbol":`, 1), strings.Replace(valid, `"quantity":"1"`, `"quantity":1`, 1), strings.Replace(valid, `"quantity":"1"`, `"quantity":"1.5"`, 1), strings.Replace(valid, `"side":"buy"`, `"side":"invalid"`, 1), strings.Replace(valid, `"type":"limit"`, `"type":"invalid"`, 1), strings.Replace(valid, buyer.String(), uuid.Nil.String(), 1)}
	for i, body := range cases {
		if response := orderHTTPCall(handler, body, "invalid"); response.Code != 400 {
			t.Fatalf("case %d accepted: %d %s", i, response.Code, response.Body)
		}
	}
	if response := orderHTTPCall(handler, strings.Repeat("x", 4097), "large"); response.Code != 413 {
		t.Fatal(response.Code)
	}
	if response := orderHTTPCall(handler, valid, ""); response.Code != 400 {
		t.Fatal(response.Code)
	}
	for _, tc := range []struct {
		method, path, contentType string
		want                      int
	}{{"GET", "/orders", "application/json", 405}, {"POST", "/other", "application/json", 404}, {"POST", "/orders", "text/plain", 415}} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(valid))
		request.Header.Set("Content-Type", tc.contentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatalf("unexpected protocol status: %d", response.Code)
		}
	}
	if engine.GetStats().OrdersProcessed != 0 {
		t.Fatal("invalid JSON mutated the engine")
	}
}

func TestHTTPOrderBackpressureCancellationAndShutdown(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	handler, _ := NewOrderHTTPHandler(engine)
	body := httpOrderBody(t, derivativeTestOrder(buyer, Bid, 1))
	if err := engine.SetOrderRequestCapacity(1); err != nil {
		t.Fatal(err)
	}
	if response := orderHTTPCall(handler, body, "first"); response.Code != 200 {
		t.Fatal(response.Code)
	}
	if response := orderHTTPCall(handler, httpOrderBody(t, derivativeTestOrder(buyer, Bid, 1)), "second"); response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatal("capacity failure did not produce retryable backpressure")
	}
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "first")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(ctx))
	if response.Code != 408 {
		t.Fatal(response.Code)
	}
	engine.Close()
	if response := orderHTTPCall(handler, body, "first"); response.Code != 503 {
		t.Fatal(response.Code)
	}
	if _, err := NewOrderHTTPHandler(nil); err == nil {
		t.Fatal("nil engine accepted")
	}
	legacy := NewMatchingEngine(1, 1)
	defer legacy.Close()
	if _, err := NewOrderHTTPHandler(legacy); err == nil {
		t.Fatal("educational risk profile exposed over HTTP")
	}
}

type unreadableOrderBody struct{}

func (unreadableOrderBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (unreadableOrderBody) Close() error             { return nil }

func TestHTTPOrderRejectsUnreadableBodyAndDuplicateKeyHeaders(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	handler, _ := NewOrderHTTPHandler(engine)
	request := httptest.NewRequest(http.MethodPost, "/orders", unreadableOrderBody{})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatal(response.Code)
	}
	request.Header.Add("Idempotency-Key", "other")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatal(response.Code)
	}
}

func TestHTTPUncertainOutcomeCanBeResolvedByReplay(t *testing.T) {
	engine, buyer, _ := newFuturesPnLTest(t, 1000, 1)
	handler, _ := NewOrderHTTPHandler(engine)
	body := httpOrderBody(t, derivativeTestOrder(buyer, Bid, 1))
	release := holdMatchingLoop(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "uncertain")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, request); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(engine.commandChan) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("HTTP order did not enqueue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if response.Code != 504 {
		t.Fatalf("uncertain outcome returned %d", response.Code)
	}
	release()
	replay := orderHTTPCall(handler, body, "uncertain")
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || engine.GetStats().OrdersProcessed != 1 {
		t.Fatal("HTTP retry did not resolve original outcome")
	}
}

func TestHTTPOrderTypeConversionPreservesContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind OrderType
	}{{"limit", Limit}, {"market", Market}, {"stop_market", StopMarket}, {"stop_limit", StopLimit}} {
		request := HTTPOrderRequest{OrderID: uuid.New(), ClientID: uuid.New(), Symbol: "SIM-FUT", Side: "sell", Type: tc.name, Quantity: 1, Price: 100, StopPrice: 200}
		order, err := request.order()
		if err != nil || order.Type != tc.kind || order.Side != Ask || order.Remaining != 1 {
			t.Fatalf("invalid HTTP conversion: %+v %v", order, err)
		}
	}
	request := HTTPOrderRequest{OrderID: uuid.New(), ClientID: uuid.New(), Symbol: "SIM-FUT", Side: "buy", Type: "limit", Quantity: 1}
	if _, err := request.order(); err == nil {
		t.Fatal("missing limit price accepted")
	}
}
