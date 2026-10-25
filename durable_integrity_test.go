package main

import (
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func TestDurableRecoveryRejectsIncoherentState(t *testing.T) {
	engine, _, buyer, seller := newDurableFuturesTest(t)
	ask, bid := derivativeTestOrder(seller, Ask, 2), derivativeTestOrder(buyer, Bid, 1)
	ask.Price = 100
	bid.Price = 100
	durableSubmit(t, engine, "maker", ask)
	durableSubmit(t, engine, "taker", bid)
	resting := derivativeTestOrder(buyer, Bid, 1)
	resting.Price = 50
	durableSubmit(t, engine, "resting", resting)
	stop := derivativeTestOrder(buyer, Bid, 1)
	stop.Type = StopMarket
	stop.StopPrice = 1000
	durableSubmit(t, engine, "stop", stop)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "ALT", MarginCurrency: "SIM", InitialMarginPerContract: 100}); err != nil {
		t.Fatal(err)
	}
	alt := derivativeTestOrder(buyer, Bid, 1)
	alt.Symbol = "ALT"
	durableSubmit(t, engine, "alt", alt)
	engine.mu.Lock()
	baseline := append([]byte(nil), engine.lastState...)
	engine.mu.Unlock()
	engine.Close()
	cases := []struct {
		name    string
		corrupt func(*durableState)
	}{
		{"version", func(s *durableState) { s.Version = 2 }},
		{"instrument", func(s *durableState) {
			i := s.Instruments["SIM-FUT"]
			i.InitialMarginPerContract = 0
			s.Instruments["SIM-FUT"] = i
		}},
		{"client", func(s *durableState) { s.Clients[buyer].ID = uuid.Nil }},
		{"mark", func(s *durableState) { s.Marks["SIM-FUT"] = 0 }},
		{"limits", func(s *durableState) { s.Limits[buyer] = nil }},
		{"breaker", func(s *durableState) { s.Breakers["SIM-FUT"] = nil }},
		{"request", func(s *durableState) {
			r := s.Requests["maker"]
			r.Receipt.FilledQuantity = 999
			s.Requests["maker"] = r
		}},
		{"duplicate_request_identity", func(s *durableState) {
			r := s.Requests["maker"]
			r.Receipt.Sequence = s.Requests["taker"].Receipt.Sequence
			s.Requests["taker"] = r
		}},
		{"book_order", func(s *durableState) { s.Books["SIM-FUT"][0].Type = Market }},
		{"duplicate_book_order", func(s *durableState) { s.Books["SIM-FUT"] = append(s.Books["SIM-FUT"], s.Books["SIM-FUT"][0]) }},
		{"unrecorded_book_order", func(s *durableState) { s.Books["SIM-FUT"][0].ID = uuid.New() }},
		{"mismatched_book_order", func(s *durableState) { s.Books["SIM-FUT"][0].Quantity++ }},
		{"stop", func(s *durableState) { s.Stops["SIM-FUT"][0].StopPrice = 0 }},
		{"missing_reservations", func(s *durableState) { s.Reservations = nil }},
		{"reservation", func(s *durableState) { s.Reservations[0].Reservation.Remaining++ }},
		{"position_client", func(s *durableState) { s.Positions[uuid.New()] = map[string]DerivativePosition{} }},
		{"position_pending", func(s *durableState) {
			p := s.Positions[buyer]["SIM-FUT"]
			p.OpenBuyContracts++
			s.Positions[buyer]["SIM-FUT"] = p
		}},
		{"invalid_lot", func(s *durableState) { s.Lots[buyer]["SIM-FUT"][0].Quantity = math.MaxUint64 }},
		{"lot_balance", func(s *durableState) { s.Lots[buyer]["SIM-FUT"][0].Quantity = 2 }},
		{"missing_pending_position", func(s *durableState) { delete(s.Positions[buyer], "ALT") }},
		{"unreferenced_lot", func(s *durableState) { s.Lots[buyer]["UNKNOWN"] = []DerivativeLot{{Quantity: 1, EntryPrice: 100}} }},
		{"net_conservation", func(s *durableState) {
			p := s.Positions[buyer]["SIM-FUT"]
			p.NetContracts = 2
			s.Positions[buyer]["SIM-FUT"] = p
			s.Lots[buyer]["SIM-FUT"][0].Quantity = 2
		}},
		{"account_overflow", func(s *durableState) {
			p := s.Positions[buyer]["SIM-FUT"]
			p.RealizedPnL = math.MaxInt64
			s.Positions[buyer]["SIM-FUT"] = p
		}},
		{"trade_instrument", func(s *durableState) { s.Outbox[0].Trade.Symbol = "UNKNOWN" }},
		{"trade_sequence", func(s *durableState) { s.Outbox[0].Sequence = 2 }},
		{"policy", func(s *durableState) { s.LiquidationPolicy.MaxSlippageBps = 10001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var state durableState
			if err := json.Unmarshal(baseline, &state); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(&state)
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "invalid.db")
			db, err := bolt.Open(path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeDurableState(db, payload); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if recovered, err := NewDurableDerivativesMatchingEngine(path, 4, 4); !errors.Is(err, ErrInvalidDurableState) {
				if recovered != nil {
					recovered.Close()
				}
				t.Fatalf("incoherent snapshot accepted: %v", err)
			}
			unlocked, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal("rejected recovery leaked database lock", err)
			}
			unlocked.Close()
		})
	}
}

func TestDurableStoreRejectsCorruptAndUnrelatedData(t *testing.T) {
	for _, kind := range []string{"truncated", "checksum", "json", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.db")
			db, err := bolt.Open(path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bolt.Tx) error {
				if kind == "unrelated" {
					_, err := tx.CreateBucket([]byte("other-application"))
					return err
				}
				bucket, err := tx.CreateBucket(durableBucket)
				if err != nil {
					return err
				}
				switch kind {
				case "truncated":
					return bucket.Put(durableStateKey, []byte("OME1"))
				case "checksum":
					return bucket.Put(durableStateKey, append([]byte("OME1"), make([]byte, 40)...))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "json" {
				if err := writeDurableState(db, []byte("{")); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			if engine, err := NewDurableDerivativesMatchingEngine(path, 4, 4); !errors.Is(err, ErrInvalidDurableState) {
				if engine != nil {
					engine.Close()
				}
				t.Fatal("corrupt database accepted", err)
			}
		})
	}
	if err := writeDurableState(nil, make([]byte, maxDurableStateBytes+1)); !errors.Is(err, ErrInvalidDurableState) {
		t.Fatal("oversized snapshot accepted", err)
	}
	path := filepath.Join(t.TempDir(), "not-a-db")
	if err := os.WriteFile(path, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if engine, err := NewDurableDerivativesMatchingEngine(path, 4, 4); err == nil {
		engine.Close()
		t.Fatal("non-database file accepted")
	}
}
