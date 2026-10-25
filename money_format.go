package main

import "fmt"

const demoMoneyScale uint64 = 1_000_000

func formatDemoMoney(value uint64) string {
	return fmt.Sprintf("%d.%06d", value/demoMoneyScale, value%demoMoneyScale)
}
