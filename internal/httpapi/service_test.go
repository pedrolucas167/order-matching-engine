package httpapi

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

const serviceTestToken = "test-only-operator-token-0123456789abcdef"

func serviceCall(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Idempotency-Key", "service-"+body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestServiceAuthenticationAndHealth(t *testing.T) {
	engine, _, _, _ := newDurableHTTPFixture(t)
	handler, err := NewServiceHandler(engine, serviceTestToken)
	if err != nil {
		t.Fatal(err)
	}
	if response := serviceCall(handler, "GET", "/healthz", "", ""); response.Code != 200 {
		t.Fatal(response.Code)
	}
	for _, token := range []string{"", "incorrect"} {
		if response := serviceCall(handler, "GET", "/trades/state", "", token); response.Code != 401 || response.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("unauthenticated financial access accepted")
		}
	}
	if response := serviceCall(handler, "POST", "/healthz", "", ""); response.Code != 405 {
		t.Fatal(response.Code)
	}
	if response := serviceCall(handler, "GET", "/unknown", "", serviceTestToken); response.Code != 404 {
		t.Fatal(response.Code)
	}
	request := httptest.NewRequest("GET", "/trades/state", nil)
	request.Header.Add("Authorization", "Bearer "+serviceTestToken)
	request.Header.Add("Authorization", "Bearer "+serviceTestToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("duplicate authorization accepted")
	}
	if _, err := NewServiceHandler(nil, serviceTestToken); err == nil {
		t.Fatal("nil engine accepted")
	}
	for _, token := range []string{"short", strings.Repeat("x", 4097), " " + serviceTestToken} {
		if _, err := NewServiceHandler(engine, token); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	engine.Close()
	if response := serviceCall(handler, "GET", "/healthz", "", ""); response.Code != 503 {
		t.Fatal(response.Code)
	}
}

func TestServiceOrdersQueriesMarkingAndTradeAcknowledgment(t *testing.T) {
	engine, _, buyer, seller := newDurableHTTPFixture(t)
	handler, _ := NewServiceHandler(engine, serviceTestToken)
	ask := derivativeTestOrder(seller, matching.Ask, 1)
	ask.Price = 100
	bid := derivativeTestOrder(buyer, matching.Bid, 1)
	bid.Price = 100
	for _, order := range []string{httpOrderBody(t, ask), httpOrderBody(t, bid)} {
		request := httptest.NewRequest("POST", "/orders", strings.NewReader(order))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+serviceTestToken)
		request.Header.Set("Idempotency-Key", uuid.NewString())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body)
		}
	}
	if response := serviceCall(handler, "POST", "/marks", `{"symbol":"SIM-FUT","price":"110"}`, serviceTestToken); response.Code != 204 {
		t.Fatal(response.Code, response.Body)
	}
	margin := serviceCall(handler, "GET", "/accounts/"+buyer.String()+"/margin", "", serviceTestToken)
	var account marginResponse
	if err := json.Unmarshal(margin.Body.Bytes(), &account); err != nil || margin.Code != 200 || account.Equity != 1010 || account.MarginCurrency != "SIM" {
		t.Fatalf("incorrect financial response: %s %v", margin.Body, err)
	}
	position := serviceCall(handler, "GET", "/accounts/"+buyer.String()+"/positions/SIM-FUT", "", serviceTestToken)
	if position.Code != 200 || !strings.Contains(position.Body.String(), `"net_contracts":"1"`) {
		t.Fatal(position.Code, position.Body)
	}
	if response := serviceCall(handler, "GET", "/books/SIM-FUT", "", serviceTestToken); response.Code != 200 {
		t.Fatal(response.Code)
	}
	events := serviceCall(handler, "GET", "/trades?after=0&limit=1", "", serviceTestToken)
	var trades []tradeEventResponse
	if err := json.Unmarshal(events.Body.Bytes(), &trades); err != nil || events.Code != 200 || len(trades) != 1 || trades[0].Trade.Price != 100 {
		t.Fatalf("incorrect outbox response: %s %v", events.Body, err)
	}
	if response := serviceCall(handler, "POST", "/trades/ack", `{"sequence":"1"}`, serviceTestToken); response.Code != 204 {
		t.Fatal(response.Code, response.Body)
	}
	state := serviceCall(handler, "GET", "/trades/state", "", serviceTestToken)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"acknowledged":"1"`) {
		t.Fatal(state.Code, state.Body)
	}
	if response := serviceCall(handler, "GET", "/trades?after=1", "", serviceTestToken); response.Code != 200 || response.Body.String() != "[]" {
		t.Fatal(response.Code, response.Body)
	}
}

func TestServiceCancellationAndQuotes(t *testing.T) {
	engine, _, buyer, seller := newDurableHTTPFixture(t)
	handler, _ := NewServiceHandler(engine, serviceTestToken)
	for _, side := range []matching.Side{matching.Bid, matching.Ask} {
		order := derivativeTestOrder(buyer, matching.Bid, 1)
		if side == matching.Ask {
			order.ClientID = seller
			order.Side = matching.Ask
			order.Price = 20000
		}
		request := httptest.NewRequest("POST", "/orders", strings.NewReader(httpOrderBody(t, order)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+serviceTestToken)
		request.Header.Set("Idempotency-Key", order.ID.String())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body)
		}
		book := serviceCall(handler, "GET", "/books/SIM-FUT", "", serviceTestToken)
		if book.Code != 200 || !strings.Contains(book.Body.String(), `"quantity":"1"`) {
			t.Fatal(book.Code, book.Body)
		}
		if response := serviceCall(handler, "DELETE", "/orders/SIM-FUT/"+order.ID.String(), "", serviceTestToken); response.Code != 204 {
			t.Fatal(response.Code, response.Body)
		}
	}
	stop := derivativeTestOrder(buyer, matching.Bid, 1)
	stop.Type = matching.StopMarket
	stop.StopPrice = 20000
	request := httptest.NewRequest("POST", "/orders", strings.NewReader(httpOrderBody(t, stop)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+serviceTestToken)
	request.Header.Set("Idempotency-Key", stop.ID.String())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	if response := serviceCall(handler, "DELETE", "/orders/SIM-FUT/"+stop.ID.String(), "", serviceTestToken); response.Code != 204 {
		t.Fatal(response.Code, response.Body)
	}
}

func TestServiceRejectsInvalidOperationsAndClosedEngine(t *testing.T) {
	engine, _, buyer, _ := newDurableHTTPFixture(t)
	handler, _ := NewServiceHandler(engine, serviceTestToken)
	paths := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/accounts/invalid/margin", "", 400}, {"GET", "/accounts/" + uuid.NewString() + "/margin", "", 404}, {"GET", "/accounts/invalid/positions/SIM-FUT", "", 400}, {"GET", "/accounts/" + buyer.String() + "/positions/UNKNOWN", "", 404}, {"GET", "/books/UNKNOWN", "", 404},
		{"DELETE", "/orders/SIM-FUT/invalid", "", 400}, {"DELETE", "/orders/SIM-FUT/" + uuid.NewString(), "", 404},
		{"POST", "/marks", `{}`, 400}, {"POST", "/marks", `{"symbol":"SIM-FUT","price":"0"}`, 400}, {"POST", "/marks", `{"symbol":"UNKNOWN","price":"1"}`, 404},
		{"POST", "/trades/ack", `{}`, 400}, {"POST", "/trades/ack", `{"sequence":"999"}`, 400}, {"POST", "/trades/ack", `null`, 400}, {"POST", "/trades/ack", `{`, 400},
		{"GET", "/trades?after=x", "", 400}, {"GET", "/trades?limit=x", "", 400}, {"GET", "/trades?after=0&after=1", "", 400}, {"GET", "/trades?limit=0", "", 400},
	}
	for _, tc := range paths {
		if response := serviceCall(handler, tc.method, tc.path, tc.body, serviceTestToken); response.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, response.Code, response.Body)
		}
	}
	for _, path := range []string{"/accounts/" + buyer.String() + "/margin", "/accounts/" + buyer.String() + "/positions/SIM-FUT", "/books/SIM-FUT", "/orders/SIM-FUT/x", "/marks", "/trades", "/trades/state", "/trades/ack"} {
		if response := serviceCall(handler, "PATCH", path, "", serviceTestToken); response.Code != 405 {
			t.Fatalf("method accepted: %s %d", path, response.Code)
		}
	}
	for _, tc := range []struct {
		body, contentType string
		want              int
	}{{"{}", "text/plain", 415}, {strings.Repeat("x", 4097), "application/json", 413}} {
		request := httptest.NewRequest("POST", "/marks", strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer "+serviceTestToken)
		request.Header.Set("Content-Type", tc.contentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatal(response.Code)
		}
	}
	request := httptest.NewRequest("POST", "/marks", unreadableOrderBody{})
	request.Header.Set("Authorization", "Bearer "+serviceTestToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatal(response.Code)
	}
	response = httptest.NewRecorder()
	writeServiceJSON(response, make(chan int))
	if response.Code != 500 {
		t.Fatal(response.Code)
	}
	engine.Close()
	for _, tc := range []struct{ method, path, body string }{{"GET", "/accounts/" + buyer.String() + "/margin", ""}, {"GET", "/accounts/" + buyer.String() + "/positions/SIM-FUT", ""}, {"GET", "/books/SIM-FUT", ""}, {"DELETE", "/orders/SIM-FUT/" + uuid.NewString(), ""}, {"POST", "/marks", `{"symbol":"SIM-FUT","price":"1"}`}, {"GET", "/trades", ""}, {"GET", "/trades/state", ""}, {"POST", "/trades/ack", `{"sequence":"0"}`}} {
		if response := serviceCall(handler, tc.method, tc.path, tc.body, serviceTestToken); response.Code != 503 {
			t.Fatalf("closed engine accepted %s: %d", tc.path, response.Code)
		}
	}
}

func TestServiceConcurrentTradeQueriesDoNotShareParseState(t *testing.T) {
	engine, _, _, _ := newDurableHTTPFixture(t)
	handler, _ := NewServiceHandler(engine, serviceTestToken)
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if response := serviceCall(handler, "GET", "/trades?after=0&limit=1", "", serviceTestToken); response.Code != 200 {
				t.Error(response.Code)
			}
		}()
	}
	group.Wait()
}
