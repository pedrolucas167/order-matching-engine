package server

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func containerCommand(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = append(os.Environ(), "ORDER_API_TOKEN="+testOperatorToken)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker operation failed: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestContainerServicePersistsAcrossReplacement(t *testing.T) {
	image := os.Getenv("ORDER_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set ORDER_CONTAINER_IMAGE to run the real container integration test")
	}
	config := testConfig()
	path := writeTestConfig(t, config)
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	name := "ome-test-" + uuid.NewString()
	volume := "ome-test-data-" + uuid.NewString()
	containerCommand(t, "volume", "create", volume)
	t.Cleanup(func() {
		command := exec.Command("docker", "volume", "rm", volume)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("volume cleanup failed: %v %s", err, output)
		}
	})
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	start := func() string {
		containerCommand(t, "run", "-d", "--name", name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--env", "ORDER_API_TOKEN", "--mount", "type=volume,source="+volume+",target=/data", "--mount", "type=bind,source="+filepath.Dir(path)+",target=/config,readonly", "-p", "127.0.0.1::8080", image)
		address := containerCommand(t, "port", name, "8080/tcp")
		endpoint := "http://" + strings.TrimSpace(strings.Split(address, "\n")[0])
		deadline := time.Now().Add(20 * time.Second)
		client := &http.Client{Timeout: time.Second}
		for {
			response, err := client.Get(endpoint + "/healthz")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("container did not become healthy: ", containerCommand(t, "logs", name))
			}
			time.Sleep(50 * time.Millisecond)
		}
		if user := containerCommand(t, "inspect", "--format", "{{.Config.User}}", name); user != "65532:65532" {
			t.Fatal("container does not run as the expected non-root user", user)
		}
		containerCommand(t, "exec", name, "/order-server", "healthcheck")
		return endpoint
	}
	endpoint := start()
	client := &http.Client{Timeout: time.Second}
	unauthenticated, err := client.Get(endpoint + "/trades/state")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != 401 {
		t.Fatal("container accepts financial access without credentials")
	}
	ask := fmt.Sprintf(`{"order_id":%q,"client_id":%q,"symbol":"SIM-FUT","side":"sell","type":"limit","quantity":"1","price":"100"}`, uuid.NewString(), config.Clients[1].ID.String())
	bid := fmt.Sprintf(`{"order_id":%q,"client_id":%q,"symbol":"SIM-FUT","side":"buy","type":"limit","quantity":"1","price":"100"}`, uuid.NewString(), config.Clients[0].ID.String())
	if status, body, _ := serviceRequest(t, "POST", endpoint+"/orders", ask, "maker"); status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, _ := serviceRequest(t, "POST", endpoint+"/orders", bid, "taker")
	if status != 200 || !strings.Contains(receipt, `"initial_status":"filled"`) {
		t.Fatal(status, receipt)
	}
	if status, body, _ := serviceRequest(t, "POST", endpoint+"/marks", `{"symbol":"SIM-FUT","price":"110"}`, ""); status != 204 {
		t.Fatal(status, body)
	}
	if status, body, _ := serviceRequest(t, "POST", endpoint+"/trades/ack", `{"sequence":"1"}`, ""); status != 204 {
		t.Fatal(status, body)
	}
	containerCommand(t, "stop", "--time", "10", name)
	if exit := containerCommand(t, "inspect", "--format", "{{.State.ExitCode}}", name); exit != "0" {
		t.Fatal("container did not shut down gracefully", exit)
	}
	if logs := containerCommand(t, "logs", name); strings.Contains(logs, "Demo") || strings.Contains(logs, testOperatorToken) {
		t.Fatal("container runs demo scenarios or logs credentials")
	}
	containerCommand(t, "rm", name)
	config.Clients[0].InitialCollateral = 1
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint = start()
	status, replayed, headers := serviceRequest(t, "POST", endpoint+"/orders", bid, "taker")
	if status != 200 || replayed != receipt || headers.Get("Idempotency-Replayed") != "true" {
		t.Fatal("container replacement lost idempotency", status, replayed)
	}
	status, account, _ := serviceRequest(t, "GET", endpoint+"/accounts/"+config.Clients[0].ID.String()+"/margin", "", "")
	if status != 200 || !strings.Contains(account, `"collateral":"1000"`) || !strings.Contains(account, `"equity":"1010"`) {
		t.Fatal("container replacement reset financial state", account)
	}
	status, state, _ := serviceRequest(t, "GET", endpoint+"/trades/state", "", "")
	if status != 200 || !strings.Contains(state, `"acknowledged":"1"`) || !strings.Contains(state, `"pending":0`) {
		t.Fatal("container replacement lost delivery acknowledgment", state)
	}
	t.Log("real container replacement preserved receipt, position, margin, mark and trade acknowledgment")
}
