package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testOperatorToken = "test-only-operator-token-0123456789abcdef"

type startupWriter struct{ lines chan string }

func (writer startupWriter) Write(data []byte) (int, error) {
	writer.lines <- strings.TrimSpace(string(data))
	return len(data), nil
}

func startTestService(t *testing.T, args []string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	lines := make(chan string, 4)
	go func() { done <- Run(ctx, args, log.New(startupWriter{lines}, "", 0)) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("service did not stop")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case line := <-lines:
		return "http://" + strings.TrimPrefix(line, "order service listening on "), stop
	case err := <-done:
		cancel()
		once.Do(func() {})
		t.Fatalf("service failed to start: %v", err)
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("service startup timed out")
	}
	return "", stop
}

func serviceRequest(t *testing.T, method, endpoint, body, key string) (int, string, http.Header) {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testOperatorToken)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data), response.Header
}

func TestServiceRunPersistsOrdersAndDoesNotResetFunding(t *testing.T) {
	t.Setenv("ORDER_API_TOKEN", testOperatorToken)
	config := testConfig()
	path := writeTestConfig(t, config)
	db := filepath.Join(t.TempDir(), "engine.db")
	args := []string{"-listen", "127.0.0.1:0", "-config", path, "-db", db}
	endpoint, stop := startTestService(t, args)
	if status, _, _ := serviceRequest(t, "GET", endpoint+"/healthz", "", ""); status != 200 {
		t.Fatal(status)
	}
	order := `{"order_id":"a79c1d57-0b06-4de7-bc83-00802d7c9ac0","client_id":"` + config.Clients[0].ID.String() + `","symbol":"SIM-FUT","side":"buy","type":"limit","quantity":"1","price":"100"}`
	status, receipt, _ := serviceRequest(t, "POST", endpoint+"/orders", order, "persistent-order")
	if status != 200 {
		t.Fatal(status, receipt)
	}
	stop()
	config.Clients[0].InitialCollateral = 1
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, stop = startTestService(t, args)
	defer stop()
	status, replayed, headers := serviceRequest(t, "POST", endpoint+"/orders", order, "persistent-order")
	if status != 200 || replayed != receipt || headers.Get("Idempotency-Replayed") != "true" {
		t.Fatal("service restart duplicated order")
	}
	status, account, _ := serviceRequest(t, "GET", endpoint+"/accounts/"+config.Clients[0].ID.String()+"/margin", "", "")
	if status != 200 || !strings.Contains(account, `"collateral":"1000"`) {
		t.Fatal("service restart reapplied initial collateral", account)
	}
}

func TestRunRejectsInvalidOptionsAndSecrets(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	if err := Run(context.Background(), []string{"-shutdown-timeout", "0s"}, logger); err == nil {
		t.Fatal("nonpositive shutdown deadline was accepted")
	}
	config := writeTestConfig(t, testConfig())
	db := filepath.Join(t.TempDir(), "engine.db")
	t.Setenv("ORDER_API_TOKEN", testOperatorToken)
	for _, args := range [][]string{{"-unknown"}, {"extra"}, {"-config", filepath.Join(t.TempDir(), "missing")}, {"-config", config, "-db", db, "-order-buffer", "0"}, {"-config", config, "-db", db, "-listen", "invalid"}, {"-config", config, "-db", filepath.Join(t.TempDir(), "missing", "db")}, {"-config", config, "-token-file", filepath.Join(t.TempDir(), "missing")}} {
		if err := Run(context.Background(), args, logger); err == nil {
			t.Fatalf("invalid startup accepted: %v", args)
		}
	}
	if err := Run(context.Background(), []string{"--help"}, logger); err != nil {
		t.Fatal(err)
	}
	if err := Run(nil, nil, logger); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := Run(context.Background(), nil, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, nil, logger); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, token := range []string{"", "short", " " + testOperatorToken, strings.Repeat("x", 4097)} {
		t.Setenv("ORDER_API_TOKEN", token)
		if err := Run(context.Background(), []string{"-config", config, "-db", db}, logger); err == nil {
			t.Fatal("invalid operator token accepted")
		}
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"-config", config, "-token-file", tokenFile}, logger); err == nil {
		t.Fatal("oversized token file accepted")
	}
	if err := Run(context.Background(), []string{"-config", config, "-token-file", t.TempDir()}, logger); err == nil {
		t.Fatal("token directory accepted")
	}
	if err := os.WriteFile(tokenFile, []byte(testOperatorToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, stop := startTestService(t, []string{"-listen", "127.0.0.1:0", "-config", config, "-db", db, "-token-file", tokenFile})
	stop()
}

func TestRunRejectsConflictingExistingConfiguration(t *testing.T) {
	t.Setenv("ORDER_API_TOKEN", testOperatorToken)
	config := testConfig()
	path := writeTestConfig(t, config)
	db := filepath.Join(t.TempDir(), "engine.db")
	_, stop := startTestService(t, []string{"-listen", "127.0.0.1:0", "-config", path, "-db", db})
	stop()
	config.Instruments[0].InitialMargin = 200
	encoded, _ := json.Marshal(config)
	os.WriteFile(path, encoded, 0600)
	if err := Run(context.Background(), []string{"-config", path, "-db", db}, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("conflicting existing margin rules were overwritten")
	}
}

func TestHealthcheckAndServeErrors(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	if err := Run(context.Background(), []string{"healthcheck", "-url", server.URL}, logger); err != nil {
		t.Fatal(err)
	}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer failed.Close()
	for _, args := range [][]string{{"-url", failed.URL}, {"-url", "://"}, {"-unknown"}, {"extra"}} {
		if err := healthcheck(args); err == nil {
			t.Fatal("invalid healthcheck passed")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	if err := serve(context.Background(), listener, http.NotFoundHandler(), time.Second); err == nil {
		t.Fatal("closed listener accepted")
	}
}

func TestServeClosesStalledConnectionsAfterShutdownDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }), 10*time.Millisecond)
	}()
	requestDone := make(chan struct{})
	go func() {
		client := &http.Client{Timeout: time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stalled request did not hit shutdown deadline", err)
	}
	<-requestDone
}
