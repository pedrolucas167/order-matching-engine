package httpapi

import (
	"net/http"
	"testing"

	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func TestDurableHTTPReceiptSurvivesRestart(t *testing.T) {
	engine, path, buyer, _ := newDurableHTTPFixture(t)
	handler, err := NewDurableOrderHTTPHandler(engine)
	if err != nil {
		t.Fatal(err)
	}
	body := httpOrderBody(t, derivativeTestOrder(buyer, matching.Bid, 1))
	first := orderHTTPCall(handler, body, "http-request")
	if first.Code != http.StatusOK {
		t.Fatal(first.Code, first.Body)
	}
	engine.Close()
	recovered := reopenDurableHTTPFixture(t, path)
	handler, err = NewDurableOrderHTTPHandler(recovered)
	if err != nil {
		t.Fatal(err)
	}
	second := orderHTTPCall(handler, body, "http-request")
	if second.Code != 200 || second.Header().Get("Idempotency-Replayed") != "true" || second.Body.String() != first.Body.String() {
		t.Fatal("HTTP recovery changed original receipt")
	}
	if _, err := NewDurableOrderHTTPHandler(nil); err == nil {
		t.Fatal("nil durable engine accepted")
	}
}
