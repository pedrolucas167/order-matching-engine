package matching

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestFuturesMaintenanceCancelsOrdersAndAllowsClosing(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 500, 1)
	manager := engine.GetRiskManager()
	manager.SetRiskLimits(seller, &RiskLimits{MaxOrderSize: 10, MaxDailyVolume: 2, MaxPositionSize: 10})
	futuresPnLTrade(t, engine, buyer, seller, 2, 100)
	pending := derivativeTestOrder(seller, Ask, 1)
	pending.Price = 1000
	stop := derivativeTestOrder(seller, Bid, 1)
	stop.Type = StopMarket
	stop.StopPrice = 2000
	submitTestOrder(t, engine, pending)
	submitTestOrder(t, engine, stop)
	synchronizeEngine(t, engine)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 320); err != nil {
		t.Fatal(err)
	}
	account, _ := manager.GetMarginAccount(seller)
	if account.Equity != 60 || account.InitialMarginShortfall != 140 || account.MaintenanceMarginShortfall != 100 || account.OrderMargin != 0 {
		t.Fatalf("maintenance deficit not exposed: %+v", account)
	}
	if snapshot := engine.GetSnapshot("SIM-FUT"); snapshot.BestAsk != nil || snapshot.BestBid != nil {
		t.Fatal("pending orders survived insufficient margin")
	}
	if stats := engine.GetStats(); stats.PendingStopOrders != 0 {
		t.Fatal("pending stop survived insufficient margin")
	}
	rejected := derivativeTestOrder(seller, Ask, 1)
	submitTestOrder(t, engine, rejected)
	synchronizeEngine(t, engine)
	if _, exists := engine.GetOrderBook("SIM-FUT").GetOrder(rejected.ID); exists {
		t.Fatal("risk increasing order bypassed the margin deficit")
	}
	closingAsk := derivativeTestOrder(buyer, Ask, 1)
	closingAsk.Price = 320
	closingBid := derivativeTestOrder(seller, Bid, 1)
	closingBid.Type = Market
	closingBid.Price = 0
	submitTestOrder(t, engine, closingAsk)
	submitTestOrder(t, engine, closingBid)
	if engine.GetStats().TradesProcessed != 2 {
		t.Fatal("undermargined account could not reduce its position")
	}
	readTestTrade(t, engine)
	position, _ := manager.GetDerivativePosition(seller, "SIM-FUT")
	account, _ = manager.GetMarginAccount(seller)
	if position.NetContracts != -1 || account.Equity != 60 || account.MaintenanceMarginShortfall != 20 {
		t.Fatalf("closing duplicated or discarded losses: %+v %+v", position, account)
	}
	if err := manager.AddClient(&Client{ID: seller, Collateral: 550, MarginCurrency: "SIM"}); err != nil {
		t.Fatal("partial deposit rejected", err)
	}
	account, _ = manager.GetMarginAccount(seller)
	if account.Equity != 110 || account.InitialMarginShortfall != 0 {
		t.Fatalf("deposit did not restore margin: %+v", account)
	}
}

func TestFuturesNegativeEquityRemainsAfterPositionCloses(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 100, 1)
	manager := engine.GetRiskManager()
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 350); err != nil {
		t.Fatal(err)
	}
	account, _ := manager.GetMarginAccount(seller)
	if account.Equity != -150 || account.AvailableCollateral != 0 || account.MaintenanceMarginShortfall != 230 {
		t.Fatalf("negative equity wrapped: %+v", account)
	}
	ask := derivativeTestOrder(buyer, Ask, 1)
	ask.Price = 350
	bid := derivativeTestOrder(seller, Bid, 1)
	bid.Type = Market
	bid.Price = 0
	submitTestOrder(t, engine, ask)
	submitTestOrder(t, engine, bid)
	if engine.GetStats().TradesProcessed != 2 {
		t.Fatal("negative equity prevented closing")
	}
	readTestTrade(t, engine)
	account, _ = manager.GetMarginAccount(seller)
	if account.Equity != -150 || account.RealizedPnL != -250 || account.MaintenanceMarginShortfall != 150 {
		t.Fatalf("closing forgave or wrapped the debt: %+v", account)
	}
	if err := manager.AddClient(&Client{ID: seller, Collateral: 250, MarginCurrency: "SIM"}); err != nil {
		t.Fatal(err)
	}
	account, _ = manager.GetMarginAccount(seller)
	if account.Equity != 0 || account.InitialMarginShortfall != 0 {
		t.Fatalf("deposit did not cover negative equity: %+v", account)
	}
}

func TestFuturesConcurrentMarksAndIndependentReads(t *testing.T) {
	engine, buyer, seller := newFuturesPnLTest(t, 1000, 1)
	futuresPnLTrade(t, engine, buyer, seller, 1, 100)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			if err := engine.SetDerivativeMarkPrice("SIM-FUT", uint64(90+i%21)); err != nil {
				t.Error(err)
			}
			engine.GetRiskManager().GetMarginAccount(buyer)
			lots, _ := engine.GetRiskManager().GetDerivativeLots(buyer, "SIM-FUT")
			lots[0].EntryPrice = 999
		}(i)
	}
	workers.Wait()
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 105); err != nil {
		t.Fatal(err)
	}
	first, _ := engine.GetRiskManager().GetMarginAccount(buyer)
	second, _ := engine.GetRiskManager().GetMarginAccount(seller)
	if first.Equity != 1005 || second.Equity != 995 {
		t.Fatalf("concurrent marks or reads changed the basis: %+v %+v", first, second)
	}
}

func TestFuturesValuationConfigurationAndClosedEngine(t *testing.T) {
	engine, _, _ := newFuturesPnLTest(t, 1000, 1)
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "BAD-MAINT", MarginCurrency: "SIM", InitialMarginPerContract: 100, MaintenanceMarginPerContract: 101}); !errors.Is(err, ErrInvalidDerivativeInstrument) {
		t.Fatalf("invalid maintenance returned %v", err)
	}
	if err := engine.AddDerivativeInstrument(DerivativeInstrument{Symbol: "BAD-MULT", MarginCurrency: "SIM", InitialMarginPerContract: 100, ContractMultiplier: math.MaxUint64}); !errors.Is(err, ErrInvalidDerivativeInstrument) {
		t.Fatalf("invalid multiplier returned %v", err)
	}
	if err := engine.GetRiskManager().AddClient(&Client{ID: uuid.New(), Collateral: math.MaxUint64, MarginCurrency: "SIM"}); !errors.Is(err, ErrRiskArithmeticOverflow) {
		t.Fatalf("unsupported signed-money range returned %v", err)
	}
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 0); !errors.Is(err, ErrUnknownMarkPrice) {
		t.Fatalf("zero mark returned %v", err)
	}
	if err := engine.SetDerivativeMarkPrice("UNKNOWN", 100); !errors.Is(err, ErrUnknownDerivativeInstrument) {
		t.Fatalf("unknown mark instrument returned %v", err)
	}
	engine.Close()
	if err := engine.SetDerivativeMarkPrice("SIM-FUT", 100); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("mark after shutdown returned %v", err)
	}
	if err := engine.SettleDerivativeInstrument("SIM-FUT", 100); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("settlement after shutdown returned %v", err)
	}
}
