package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func NewServiceHandler(engine *matching.DurableMatchingEngine, token string) (http.Handler, error) {
	if engine == nil || len(token) < 32 || len(token) > 4096 || strings.TrimSpace(token) != token {
		return nil, errors.New("service requires a durable engine and an operator token of 32 to 4096 characters")
	}
	orders, err := NewDurableOrderHTTPHandler(engine)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeOrderHTTPError(w, 404, "not_found") })
	mux.Handle("/orders", orders)
	mux.HandleFunc("/accounts/{client}/margin", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		id, err := uuid.Parse(r.PathValue("client"))
		if err != nil {
			writeOrderHTTPError(w, 400, "invalid_client")
			return
		}
		client, err := engine.GetClient(id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		account, err := engine.GetMarginAccount(id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeServiceJSON(w, marginResponse{ClientID: id, MarginCurrency: client.MarginCurrency, Collateral: account.Collateral, PositionMargin: account.PositionMargin, OrderMargin: account.OrderMargin, AvailableCollateral: account.AvailableCollateral, RealizedPnL: account.RealizedPnL, UnrealizedPnL: account.UnrealizedPnL, Equity: account.Equity, MaintenanceMargin: account.MaintenanceMargin, InitialMarginShortfall: account.InitialMarginShortfall, MaintenanceMarginShortfall: account.MaintenanceMarginShortfall})
	})
	mux.HandleFunc("/accounts/{client}/positions/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		id, err := uuid.Parse(r.PathValue("client"))
		if err != nil {
			writeOrderHTTPError(w, 400, "invalid_client")
			return
		}
		position, exists, err := engine.GetDerivativePosition(id, r.PathValue("symbol"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if !exists {
			writeOrderHTTPError(w, 404, "position_not_found")
			return
		}
		writeServiceJSON(w, positionResponse{Symbol: r.PathValue("symbol"), NetContracts: position.NetContracts, OpenBuyContracts: position.OpenBuyContracts, OpenSellContracts: position.OpenSellContracts, RealizedPnL: position.RealizedPnL, UnrealizedPnL: position.UnrealizedPnL})
	})
	mux.HandleFunc("/books/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		snapshot, err := engine.GetSnapshot(r.PathValue("symbol"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if snapshot == nil {
			writeOrderHTTPError(w, 404, "book_not_found")
			return
		}
		writeServiceJSON(w, bookResponse{Symbol: snapshot.Symbol, BestBid: levelResponse(snapshot.BestBid), BestAsk: levelResponse(snapshot.BestAsk)})
	})
	mux.HandleFunc("/orders/{symbol}/{order}", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodDelete) {
			return
		}
		id, err := uuid.Parse(r.PathValue("order"))
		if err != nil {
			writeOrderHTTPError(w, 400, "invalid_order_id")
			return
		}
		canceled, err := engine.CancelOrder(id, r.PathValue("symbol"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if !canceled {
			canceled, err = engine.CancelStopOrder(id, r.PathValue("symbol"))
			if err != nil {
				writeServiceError(w, err)
				return
			}
		}
		if !canceled {
			writeOrderHTTPError(w, 404, "order_not_active")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/marks", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var mark struct {
			Symbol string  `json:"symbol"`
			Price  *uint64 `json:"price,string"`
		}
		if !readServiceJSON(w, r, &mark) {
			return
		}
		if mark.Price == nil {
			writeOrderHTTPError(w, 400, "price_required")
			return
		}
		if err := engine.SetDerivativeMarkPrice(mark.Symbol, *mark.Price); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/trades/state", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		state, err := engine.GetTradeDeliveryState()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeServiceJSON(w, deliveryResponse{state.Acknowledged, state.Latest, state.Pending})
	})
	mux.HandleFunc("/trades/ack", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var ack struct {
			Sequence *uint64 `json:"sequence,string"`
		}
		if !readServiceJSON(w, r, &ack) {
			return
		}
		if ack.Sequence == nil {
			writeOrderHTTPError(w, 400, "sequence_required")
			return
		}
		if err := engine.AcknowledgeTrades(*ack.Sequence); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/trades", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		after, limit := uint64(0), uint64(100)
		var parseError error
		query := r.URL.Query()
		if len(query["after"]) > 1 || len(query["limit"]) > 1 {
			writeOrderHTTPError(w, 400, "invalid_cursor")
			return
		}
		if value, exists := query["after"]; exists {
			after, parseError = strconv.ParseUint(value[0], 10, 64)
			if parseError != nil {
				writeOrderHTTPError(w, 400, "invalid_cursor")
				return
			}
		}
		if value, exists := query["limit"]; exists {
			limit, parseError = strconv.ParseUint(value[0], 10, 16)
			if parseError != nil {
				writeOrderHTTPError(w, 400, "invalid_limit")
				return
			}
		}
		events, err := engine.ReadTrades(after, int(limit))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		response := make([]tradeEventResponse, 0, len(events))
		for _, event := range events {
			response = append(response, tradeEventResponse{Sequence: event.Sequence, Trade: tradeResponse{ID: event.Trade.ID, Symbol: event.Trade.Symbol, Price: event.Trade.Price, Quantity: event.Trade.Quantity, BidOrder: event.Trade.BidOrder, AskOrder: event.Trade.AskOrder, BidClient: event.Trade.BidClient, AskClient: event.Trade.AskClient, Timestamp: event.Trade.Timestamp}})
		}
		writeServiceJSON(w, response)
	})
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			if _, err := engine.GetStats(); err != nil {
				writeOrderHTTPError(w, 503, "unhealthy")
				return
			}
			writeServiceJSON(w, struct {
				Status string `json:"status"`
			}{"ok"})
			return
		}
		scheme, supplied, found := strings.Cut(r.Header.Get("Authorization"), " ")
		actual := sha256.Sum256([]byte(supplied))
		if len(r.Header.Values("Authorization")) != 1 || !found || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeOrderHTTPError(w, 401, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeOrderHTTPError(w, 405, "method_not_allowed")
	return false
}

func readServiceJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, mediaError := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaError != nil || mediaType != "application/json" {
		writeOrderHTTPError(w, 415, "json_required")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOrderHTTPError(w, 413, "body_too_large")
		} else {
			writeOrderHTTPError(w, 400, "invalid_body")
		}
		return false
	}
	if json.Unmarshal(data, target, json.RejectUnknownMembers(true)) != nil {
		writeOrderHTTPError(w, 400, "invalid_json")
		return false
	}
	return true
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, matching.ErrClientNotFound), errors.Is(err, matching.ErrUnknownDerivativeInstrument):
		writeOrderHTTPError(w, 404, "not_found")
	case errors.Is(err, matching.ErrDurableStorage), errors.Is(err, matching.ErrEngineClosed):
		writeOrderHTTPError(w, 503, "engine_unavailable")
	default:
		writeOrderHTTPError(w, 400, "invalid_operation")
	}
}

func writeServiceJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeOrderHTTPError(w, 500, "encoding_failed")
		return
	}
	_, _ = w.Write(data)
}

type marginResponse struct {
	ClientID                   uuid.UUID `json:"client_id"`
	MarginCurrency             string    `json:"margin_currency"`
	Collateral                 uint64    `json:"collateral,string"`
	PositionMargin             uint64    `json:"position_margin,string"`
	OrderMargin                uint64    `json:"order_margin,string"`
	AvailableCollateral        uint64    `json:"available_collateral,string"`
	RealizedPnL                int64     `json:"realized_pnl,string"`
	UnrealizedPnL              int64     `json:"unrealized_pnl,string"`
	Equity                     int64     `json:"equity,string"`
	MaintenanceMargin          uint64    `json:"maintenance_margin,string"`
	InitialMarginShortfall     uint64    `json:"initial_margin_shortfall,string"`
	MaintenanceMarginShortfall uint64    `json:"maintenance_margin_shortfall,string"`
}

type positionResponse struct {
	Symbol            string `json:"symbol"`
	NetContracts      int64  `json:"net_contracts,string"`
	OpenBuyContracts  uint64 `json:"open_buy_contracts,string"`
	OpenSellContracts uint64 `json:"open_sell_contracts,string"`
	RealizedPnL       int64  `json:"realized_pnl,string"`
	UnrealizedPnL     int64  `json:"unrealized_pnl,string"`
}
type quoteResponse struct {
	Price    uint64 `json:"price,string"`
	Quantity uint64 `json:"quantity,string"`
}
type bookResponse struct {
	Symbol  string         `json:"symbol"`
	BestBid *quoteResponse `json:"best_bid,omitempty"`
	BestAsk *quoteResponse `json:"best_ask,omitempty"`
}

func levelResponse(level *matching.PriceLevel) *quoteResponse {
	if level == nil {
		return nil
	}
	return &quoteResponse{level.Price, level.TotalQty}
}

type deliveryResponse struct {
	Acknowledged uint64 `json:"acknowledged,string"`
	Latest       uint64 `json:"latest,string"`
	Pending      int    `json:"pending"`
}
type tradeEventResponse struct {
	Sequence uint64        `json:"sequence,string"`
	Trade    tradeResponse `json:"trade"`
}
type tradeResponse struct {
	ID        uuid.UUID `json:"id"`
	Symbol    string    `json:"symbol"`
	Price     uint64    `json:"price,string"`
	Quantity  uint64    `json:"quantity,string"`
	BidOrder  uuid.UUID `json:"bid_order"`
	AskOrder  uuid.UUID `json:"ask_order"`
	BidClient uuid.UUID `json:"bid_client"`
	AskClient uuid.UUID `json:"ask_client"`
	Timestamp int64     `json:"timestamp,string"`
}
