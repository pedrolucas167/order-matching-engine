package server

import (
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func testConfig() Config {
	return Config{Instruments: []InstrumentConfig{{Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMargin: 100, MaintenanceMargin: 80, Multiplier: 1}}, Clients: []ClientConfig{{ID: uuid.New(), MarginCurrency: "SIM", InitialCollateral: 1000}, {ID: uuid.New(), MarginCurrency: "SIM", InitialCollateral: 1000}}}
}

func writeTestConfig(t *testing.T, config Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.json")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigRejectsAmbiguousAndInvalidInput(t *testing.T) {
	valid := testConfig()
	path := writeTestConfig(t, valid)
	if loaded, err := loadConfig(path); err != nil || loaded.Clients[0].ID != valid.Clients[0].ID {
		t.Fatal(err)
	}
	for _, data := range []string{`{}`, `null`, `{"unknown":1}`, `{"clients":[],"clients":[]}`, strings.Repeat(" ", (1<<20)+1)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	if _, err := loadConfig(t.TempDir()); err == nil {
		t.Fatal("directory accepted as configuration")
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing configuration accepted")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Instruments = nil }, func(c *Config) { c.Clients = nil }, func(c *Config) { c.Instruments[0].Symbol = " " }, func(c *Config) { c.Instruments = append(c.Instruments, c.Instruments[0]) },
		func(c *Config) { c.Instruments[0].InitialMargin = 0 }, func(c *Config) { c.Instruments[0].InitialMargin = math.MaxUint64 }, func(c *Config) { c.Instruments[0].MaintenanceMargin = 101 }, func(c *Config) { c.Instruments[0].Multiplier = math.MaxUint64 },
		func(c *Config) { c.Clients[0].ID = uuid.Nil }, func(c *Config) { c.Clients[1].ID = c.Clients[0].ID }, func(c *Config) { c.Clients[0].MarginCurrency = "UNKNOWN" }, func(c *Config) { c.Clients[0].InitialCollateral = math.MaxUint64 },
		func(c *Config) {
			c.Liquidation = &LiquidationConfig{MaxSlippageBps: 10001, MaxMarkAge: "1s", RetryInterval: "1s"}
		},
	} {
		config := testConfig()
		mutate(&config)
		if err := config.validate(); err == nil {
			t.Fatal("invalid business configuration accepted")
		}
	}
	policy, err := (LiquidationConfig{MaxSlippageBps: 100, MaxMarkAge: "1m", RetryInterval: "1s"}).policy()
	if err != nil || policy.MaxSlippageBps != 100 {
		t.Fatal(err)
	}
	if _, err := (LiquidationConfig{MaxMarkAge: "invalid", RetryInterval: "1s"}).policy(); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestBootstrapPreservesExistingFundingAndRejectsConflicts(t *testing.T) {
	engine, err := matching.NewDurableDerivativesMatchingEngine(filepath.Join(t.TempDir(), "engine.db"), 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	config := testConfig()
	config.Liquidation = &LiquidationConfig{MaxSlippageBps: 100, MaxMarkAge: "1m", RetryInterval: "1s"}
	if err := bootstrap(engine, config); err != nil {
		t.Fatal(err)
	}
	config.Clients[0].InitialCollateral = 1
	if err := bootstrap(engine, config); err != nil {
		t.Fatal(err)
	}
	client, err := engine.GetClient(config.Clients[0].ID)
	if err != nil || client.Collateral != 1000 {
		t.Fatal("bootstrap reset existing funding", err)
	}
	config.Clients[0].MarginCurrency = "OTHER"
	if err := bootstrap(engine, config); !errors.Is(err, matching.ErrMarginCurrencyMismatch) {
		t.Fatal(err)
	}
	config = testConfig()
	config.Instruments[0].InitialMargin = 0
	if err := bootstrap(engine, config); err == nil {
		t.Fatal("invalid instrument was applied")
	}
	config = testConfig()
	config.Clients[0].ID = uuid.Nil
	if err := bootstrap(engine, config); err == nil {
		t.Fatal("invalid client was applied")
	}
	config = testConfig()
	config.Liquidation = &LiquidationConfig{}
	if err := bootstrap(engine, config); err == nil {
		t.Fatal("invalid policy was applied")
	}
	engine.Close()
	if _, err := engine.GetClient(client.ID); !errors.Is(err, matching.ErrEngineClosed) {
		t.Fatal(err)
	}
	if err := bootstrap(engine, testConfig()); !errors.Is(err, matching.ErrEngineClosed) {
		t.Fatal(err)
	}
}
