package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

type OrderHTTPHandler struct{ engine *MatchingEngine }

type HTTPOrderRequest struct {
	OrderID   uuid.UUID `json:"order_id"`
	ClientID  uuid.UUID `json:"client_id"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Type      string    `json:"type"`
	Quantity  uint64    `json:"quantity,string"`
	Price     uint64    `json:"price,string,omitempty"`
	StopPrice uint64    `json:"stop_price,string,omitempty"`
}

func NewOrderHTTPHandler(engine *MatchingEngine) (http.Handler, error) {
	if engine == nil || !engine.riskManager.derivativesEnabled {
		return nil, errors.New("order HTTP handler requires a derivatives engine")
	}
	return &OrderHTTPHandler{engine: engine}, nil
}

func (handler *OrderHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/orders" {
		writeOrderHTTPError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOrderHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeOrderHTTPError(w, http.StatusUnsupportedMediaType, "json_required")
		return
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 || !validRequestKey(r.Header.Get("Idempotency-Key")) {
		writeOrderHTTPError(w, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOrderHTTPError(w, http.StatusRequestEntityTooLarge, "body_too_large")
		} else {
			writeOrderHTTPError(w, http.StatusBadRequest, "invalid_body")
		}
		return
	}
	var request HTTPOrderRequest
	if json.Unmarshal(body, &request, json.RejectUnknownMembers(true)) != nil {
		writeOrderHTTPError(w, http.StatusBadRequest, "invalid_order_json")
		return
	}
	order, err := request.order()
	if err != nil {
		writeOrderHTTPError(w, http.StatusBadRequest, "invalid_order")
		return
	}
	receipt, replayed, err := handler.engine.SubmitOrderRequest(r.Context(), r.Header.Get("Idempotency-Key"), order)
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestConflict), errors.Is(err, ErrOrderIDConflict):
			writeOrderHTTPError(w, http.StatusConflict, "request_conflict")
		case errors.Is(err, ErrRequestCapacity), errors.Is(err, ErrBackpressure):
			w.Header().Set("Retry-After", "1")
			writeOrderHTTPError(w, http.StatusTooManyRequests, "backpressure")
		case errors.Is(err, ErrRequestOutcomeUnknown):
			writeOrderHTTPError(w, http.StatusGatewayTimeout, "outcome_unknown_retry_same_key")
		case errors.Is(err, ErrEngineClosed):
			writeOrderHTTPError(w, http.StatusServiceUnavailable, "engine_closed")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeOrderHTTPError(w, http.StatusRequestTimeout, "request_cancelled")
		default:
			writeOrderHTTPError(w, http.StatusBadRequest, "invalid_order")
		}
		return
	}
	w.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	encoded, _ := json.Marshal(receipt)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (request HTTPOrderRequest) order() (*Order, error) {
	order := &Order{ID: request.OrderID, ClientID: request.ClientID, Symbol: request.Symbol, Quantity: request.Quantity, Remaining: request.Quantity, Price: request.Price, StopPrice: request.StopPrice, Status: Pending}
	if request.ClientID == uuid.Nil {
		return nil, ErrInvalidOrder
	}
	switch request.Side {
	case "buy":
		order.Side = Bid
	case "sell":
		order.Side = Ask
	default:
		return nil, ErrInvalidOrder
	}
	switch request.Type {
	case "limit":
		order.Type = Limit
	case "market":
		order.Type = Market
	case "stop_market":
		order.Type = StopMarket
	case "stop_limit":
		order.Type = StopLimit
	default:
		return nil, ErrInvalidOrder
	}
	if err := validateNewOrder(*order); err != nil {
		return nil, err
	}
	return order, nil
}

func writeOrderHTTPError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}
