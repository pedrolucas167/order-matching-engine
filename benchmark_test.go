package main

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func BenchmarkMatchingEngine_SubmitOrder(b *testing.B) {
	engine := NewMatchingEngine(10000, 10000)
	defer engine.Close()

	symbol := "BTC-USD"

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			order := &Order{
				ID:        uuid.New(),
				Symbol:    symbol,
				Side:      Bid,
				Type:      Limit,
				Price:     50000000,
				Quantity:  100,
				Remaining: 100,
				Timestamp: time.Now().UnixNano(),
			}
			engine.SubmitOrder(order)
		}
	})
}

func BenchmarkOrderBook_AddOrder(b *testing.B) {
	book := NewOrderBook("BTC-USD")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		order := &Order{
			ID:        uuid.New(),
			Symbol:    "BTC-USD",
			Side:      Bid,
			Type:      Limit,
			Price:     50000000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		book.AddOrder(order)
	}
}

func BenchmarkOrderBook_GetSnapshot(b *testing.B) {
	book := NewOrderBook("BTC-USD")

	// Populate with some orders
	for i := 0; i < 100; i++ {
		order := &Order{
			ID:        uuid.New(),
			Symbol:    "BTC-USD",
			Side:      Bid,
			Type:      Limit,
			Price:     50000000 + uint64(i)*1000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		book.AddOrder(order)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = book.GetSnapshot()
	}
}

func BenchmarkMatchingEngine_ConcurrentMatching(b *testing.B) {
	engine := NewMatchingEngine(10000, 10000)
	defer engine.Close()

	symbol := "BTC-USD"

	// Pre-populate with asks
	for i := 0; i < 1000; i++ {
		order := &Order{
			ID:        uuid.New(),
			Symbol:    symbol,
			Side:      Ask,
			Type:      Limit,
			Price:     50000000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		engine.SubmitOrder(order)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			order := &Order{
				ID:        uuid.New(),
				Symbol:    symbol,
				Side:      Bid,
				Type:      Limit,
				Price:     50000000,
				Quantity:  100,
				Remaining: 100,
				Timestamp: time.Now().UnixNano(),
			}
			engine.SubmitOrder(order)
		}
	})
}

func BenchmarkMatchingEngine_LockFreeSnapshot(b *testing.B) {
	engine := NewMatchingEngine(10000, 10000)
	defer engine.Close()

	symbol := "BTC-USD"

	// Populate with orders
	for i := 0; i < 100; i++ {
		order := &Order{
			ID:        uuid.New(),
			Symbol:    symbol,
			Side:      Bid,
			Type:      Limit,
			Price:     50000000 + uint64(i)*1000,
			Quantity:  100,
			Remaining: 100,
			Timestamp: time.Now().UnixNano(),
		}
		engine.SubmitOrder(order)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = engine.GetSnapshot(symbol)
		}
	})
}

func BenchmarkMatchingEngine_HighThroughput(b *testing.B) {
	engine := NewMatchingEngine(100000, 100000)
	defer engine.Close()

	symbol := "BTC-USD"

	b.ResetTimer()
	var wg sync.WaitGroup
	numWorkers := 10

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < b.N/numWorkers; i++ {
				side := Bid
				if i%2 == 0 {
					side = Ask
				}
				order := &Order{
					ID:        uuid.New(),
					Symbol:    symbol,
					Side:      side,
					Type:      Limit,
					Price:     50000000,
					Quantity:  100,
					Remaining: 100,
					Timestamp: time.Now().UnixNano(),
				}
				engine.SubmitOrder(order)
			}
		}(w)
	}
	wg.Wait()
}
