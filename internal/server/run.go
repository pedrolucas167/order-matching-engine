package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pedrolucas167/order-matching-engine/internal/httpapi"
	"github.com/pedrolucas167/order-matching-engine/internal/matching"
)

func Run(ctx context.Context, args []string, logger *log.Logger) error {
	if ctx == nil || logger == nil {
		return errors.New("server requires a context and logger")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(args[1:])
	}
	flags := flag.NewFlagSet("order-server", flag.ContinueOnError)
	flags.SetOutput(logger.Writer())
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	configPath := flags.String("config", "/config/engine.json", "instrument and client configuration")
	dbPath := flags.String("db", "/data/engine.db", "durable database file")
	tokenPath := flags.String("token-file", "", "read operator token from file instead of ORDER_API_TOKEN")
	orders := flags.Int("order-buffer", 512, "maximum concurrent order admissions")
	trades := flags.Int("trade-buffer", 512, "unacknowledged trade admission watermark")
	shutdownTimeout := flags.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown deadline")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *shutdownTimeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}
	config, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	token := os.Getenv("ORDER_API_TOKEN")
	if *tokenPath != "" {
		file, err := os.Open(*tokenPath)
		if err != nil {
			return errors.New("cannot read operator token file")
		}
		data, readError := io.ReadAll(io.LimitReader(file, 4097))
		file.Close()
		if readError != nil || len(data) > 4096 {
			return errors.New("operator token file exceeds 4096 bytes or cannot be read")
		}
		token = strings.TrimSpace(string(data))
	}
	if len(token) < 32 || len(token) > 4096 || strings.TrimSpace(token) != token {
		return errors.New("operator token must contain at least 32 characters without surrounding whitespace")
	}
	engine, err := matching.NewDurableDerivativesMatchingEngine(*dbPath, *orders, *trades)
	if err != nil {
		return err
	}
	defer engine.Close()
	if err := bootstrap(engine, config); err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}
	handler, err := httpapi.NewServiceHandler(engine, token)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	logger.Printf("order service listening on %s", listener.Addr())
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, listener, handler, *shutdownTimeout)
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), shutdownTimeout)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			<-finished
			return err
		}
		<-finished
		return nil
	}
}

func healthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("url", "http://127.0.0.1:8080/healthz", "health endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected healthcheck arguments")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(*endpoint)
	if err != nil {
		return errors.New("health endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("service is not healthy")
	}
	return nil
}
