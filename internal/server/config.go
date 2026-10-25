package server

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

type Config struct {
	Instruments []InstrumentConfig `json:"instruments"`
	Clients     []ClientConfig     `json:"clients"`
	Liquidation *LiquidationConfig `json:"liquidation,omitempty"`
}

type InstrumentConfig struct {
	Symbol            string `json:"symbol"`
	MarginCurrency    string `json:"margin_currency"`
	InitialMargin     uint64 `json:"initial_margin_per_contract,string"`
	MaintenanceMargin uint64 `json:"maintenance_margin_per_contract,string,omitempty"`
	Multiplier        uint64 `json:"contract_multiplier,string,omitempty"`
}

type ClientConfig struct {
	ID                uuid.UUID `json:"id"`
	MarginCurrency    string    `json:"margin_currency"`
	InitialCollateral uint64    `json:"initial_collateral,string"`
}

type LiquidationConfig struct {
	MaxSlippageBps uint64 `json:"max_slippage_bps"`
	MaxMarkAge     string `json:"max_mark_age"`
	RetryInterval  string `json:"retry_interval"`
}

func loadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("read service configuration: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, errors.New("configuration exceeds 1 MiB or cannot be read")
	}
	var config Config
	if err := json.Unmarshal(data, &config, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, errors.New("invalid service configuration JSON")
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) validate() error {
	if len(config.Instruments) == 0 || len(config.Clients) == 0 {
		return errors.New("configure at least one instrument and client")
	}
	symbols, currencies := make(map[string]bool), make(map[string]bool)
	for _, instrument := range config.Instruments {
		if instrument.Symbol == "" || strings.TrimSpace(instrument.Symbol) != instrument.Symbol || symbols[instrument.Symbol] || instrument.MarginCurrency == "" || strings.TrimSpace(instrument.MarginCurrency) != instrument.MarginCurrency || instrument.InitialMargin == 0 || instrument.InitialMargin > math.MaxInt64 || instrument.MaintenanceMargin > instrument.InitialMargin || instrument.Multiplier > math.MaxInt64 {
			return errors.New("invalid or duplicate instrument configuration")
		}
		symbols[instrument.Symbol] = true
		currencies[instrument.MarginCurrency] = true
	}
	clients := make(map[uuid.UUID]bool)
	for _, client := range config.Clients {
		if client.ID == uuid.Nil || clients[client.ID] || !currencies[client.MarginCurrency] || client.InitialCollateral > math.MaxInt64 {
			return errors.New("invalid or duplicate client configuration")
		}
		clients[client.ID] = true
	}
	if config.Liquidation != nil {
		if _, err := config.Liquidation.policy(); err != nil {
			return err
		}
	}
	return nil
}

func (config LiquidationConfig) policy() (matching.LiquidationPolicy, error) {
	age, ageError := time.ParseDuration(config.MaxMarkAge)
	retry, retryError := time.ParseDuration(config.RetryInterval)
	if ageError != nil || retryError != nil || age <= 0 || retry <= 0 || config.MaxSlippageBps > 10000 {
		return matching.LiquidationPolicy{}, errors.New("invalid liquidation configuration")
	}
	return matching.LiquidationPolicy{MaxSlippageBps: config.MaxSlippageBps, MaxMarkAge: age, RetryInterval: retry}, nil
}

func bootstrap(engine *matching.DurableMatchingEngine, config Config) error {
	for _, instrument := range config.Instruments {
		if err := engine.AddDerivativeInstrument(matching.DerivativeInstrument{Symbol: instrument.Symbol, MarginCurrency: instrument.MarginCurrency, InitialMarginPerContract: instrument.InitialMargin, MaintenanceMarginPerContract: instrument.MaintenanceMargin, ContractMultiplier: instrument.Multiplier}); err != nil {
			return err
		}
	}
	for _, client := range config.Clients {
		existing, err := engine.GetClient(client.ID)
		if errors.Is(err, matching.ErrClientNotFound) {
			if err := engine.AddClient(&matching.Client{ID: client.ID, Collateral: client.InitialCollateral, MarginCurrency: client.MarginCurrency}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if existing.MarginCurrency != client.MarginCurrency {
			return matching.ErrMarginCurrencyMismatch
		}
	}
	if config.Liquidation != nil {
		policy, err := config.Liquidation.policy()
		if err != nil {
			return err
		}
		return engine.SetLiquidationPolicy(policy)
	}
	return nil
}
