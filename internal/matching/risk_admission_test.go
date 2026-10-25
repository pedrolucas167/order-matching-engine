package matching

import (
	"testing"

	"github.com/google/uuid"
)

func TestLegacyRiskRejectionsPreserveClientState(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		exposure, price, exposureLimit uint64
		reason                         string
	}{
		{"exposure", 40, 20, 50, "exposure limit exceeded"},
		{"collateral", 0, 101, 1000, "insufficient collateral"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewRiskManager()
			defer manager.Close()
			client := Client{ID: uuid.New(), Collateral: 100, Exposure: tc.exposure}
			if err := manager.AddClient(&client); err != nil {
				t.Fatal(err)
			}
			if err := manager.SetRiskLimits(client.ID, &RiskLimits{MaxOrderSize: 10, MaxDailyVolume: 100, MaxExposure: tc.exposureLimit}); err != nil {
				t.Fatal(err)
			}
			result := manager.CheckOrder(&Order{ClientID: client.ID, Symbol: "TEST", Side: Bid, Quantity: 1, Price: tc.price})
			if result.Approved || result.Reason != tc.reason {
				t.Fatalf("risk control changed: %+v", result)
			}
			after, _ := manager.GetClient(client.ID)
			if *after != client {
				t.Fatal("rejected preview changed client funding or exposure")
			}
			stats := manager.GetStats()
			if tc.name == "exposure" && stats.ExposureBreaches != 1 {
				t.Fatal("exposure breach was not counted")
			}
			if tc.name == "collateral" && stats.CollateralBreaches != 1 {
				t.Fatal("collateral breach was not counted")
			}
		})
	}
}

func TestEngineProfilesExposeDerivativeCapability(t *testing.T) {
	legacy := NewMatchingEngine(1, 1)
	defer legacy.Close()
	derivatives := NewDerivativesMatchingEngine(1, 1)
	defer derivatives.Close()
	if legacy.IsDerivatives() || !derivatives.IsDerivatives() {
		t.Fatal("engine capability does not match its constructor")
	}
}
