package matching

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDurableCrashProcess(t *testing.T) {
	path := os.Getenv("OME_RECOVERY_TEST_DATABASE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	engine, err := NewDurableDerivativesMatchingEngine(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	var order Order
	if json.Unmarshal([]byte(os.Getenv("OME_RECOVERY_TEST_ORDER")), &order) != nil {
		t.Fatal("invalid subprocess order")
	}
	engine.mu.Lock()
	write := engine.writeState
	engine.writeState = func(payload []byte) error {
		if os.Getenv("OME_RECOVERY_TEST_STAGE") == "after_commit" {
			if err := write(payload); err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(0)
		return nil
	}
	engine.mu.Unlock()
	_, _, _ = engine.SubmitOrderRequest(context.Background(), "crashed-request", &order)
	t.Fatal("expected abrupt subprocess exit during commit")
}

func TestDurableAbruptProcessExitRecoversAtomicFinancialState(t *testing.T) {
	for _, stage := range []string{"before_commit", "after_commit"} {
		t.Run(stage, func(t *testing.T) {
			engine, path, buyer, seller := newDurableFuturesTest(t)
			ask := derivativeTestOrder(seller, Ask, 1)
			ask.Price = 100
			durableSubmit(t, engine, "maker", ask)
			engine.Close()
			bid := derivativeTestOrder(buyer, Bid, 1)
			bid.Price = 100
			encoded, err := json.Marshal(bid)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestDurableCrashProcess$")
			command.Env = append(os.Environ(), "OME_RECOVERY_TEST_DATABASE="+path, "OME_RECOVERY_TEST_ORDER="+string(encoded), "OME_RECOVERY_TEST_STAGE="+stage)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("crash helper failed: %v %s", err, output)
			}
			recovered := reopenDurableTest(t, path)
			receipt, replayed, err := recovered.SubmitOrderRequest(context.Background(), "crashed-request", bid)
			if err != nil || replayed != (stage == "after_commit") || receipt.FilledQuantity != 1 {
				t.Fatalf("process exit lost atomic recovery: %+v %v %v", receipt, replayed, err)
			}
			stats, err := recovered.GetStats()
			if err != nil || stats.TradesProcessed != 1 || stats.OrdersProcessed != 2 {
				t.Fatalf("process recovery duplicated effects: %+v %v", stats, err)
			}
			events, err := recovered.ReadTrades(0, 10)
			if err != nil || len(events) != 1 || events[0].Sequence != 1 {
				t.Fatal("process recovery lost outbox", err)
			}
		})
	}
}
