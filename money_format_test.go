package main

import (
	"math"
	"testing"
)

func TestDemoMoneyFormattingPreservesEveryStoredDigit(t *testing.T) {
	for _, tc := range []struct {
		value uint64
		want  string
	}{
		{0, "0.000000"}, {1, "0.000001"}, {999999, "0.999999"}, {1000000, "1.000000"},
		{1234567, "1.234567"}, {math.MaxUint64, "18446744073709.551615"},
	} {
		if actual := formatDemoMoney(tc.value); actual != tc.want {
			t.Errorf("format %d: got %s, want %s", tc.value, actual, tc.want)
		}
	}
}
