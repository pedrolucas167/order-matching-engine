package main

import (
	"context"
	"log"
	"os"

	"github.com/pedrolucas167/order-matching-engine/internal/server"
)

func main() {
	if err := server.Run(context.Background(), os.Args[1:], log.Default()); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
