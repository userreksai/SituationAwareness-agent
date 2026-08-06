package agent

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testSharedToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testConfig() Config {
	return Config{
		MasterURL:      "ws://127.0.0.1:9910/api/v1/agent/connect",
		AgentName:      "test-agent",
		SharedToken:    testSharedToken,
		MaxConcurrent:  2,
		DefaultTimeout: 2 * time.Second,
		MaxTimeout:     5 * time.Second,
		ReconnectMin:   10 * time.Millisecond,
		ReconnectMax:   50 * time.Millisecond,
		Heartbeat:      50 * time.Millisecond,
	}
}

func TestNormalizeTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		target    string
		wantHost  string
		wantPorts int
		wantErr   bool
	}{
		{name: "domain", target: "example.com", wantHost: "example.com", wantPorts: 2},
		{name: "URL with port", target: "http://127.0.0.1:8080/path", wantHost: "127.0.0.1", wantPorts: 1},
		{name: "bad scheme", target: "file:///etc/passwd", wantErr: true},
		{name: "credentials", target: "https://user:pass@example.com", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := normalizeTarget(test.target, nil)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTarget returned error: %v", err)
			}
			if spec.host != test.wantHost || len(spec.ports) != test.wantPorts {
				t.Fatalf("got host=%q ports=%v", spec.host, spec.ports)
			}
		})
	}
}

func TestAgentConnectsExecutesAndReturnsTask(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	result := make(chan controlMessage, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testSharedToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-Agent-Name") != "test-agent" {
			http.Error(w, "bad Agent name", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteJSON(controlMessage{Type: "registered", AgentID: "node-1", Version: "v1"})
		_ = connection.WriteJSON(controlMessage{Type: "task", TaskID: "test-1", Task: &TaskRequest{
			TaskID: "test-1",
			Type:   "probe",
			Target: target.URL,
			Options: ProbeOptions{
				TimeoutMS: 2000,
			},
		}})
		for {
			var message controlMessage
			if err := connection.ReadJSON(&message); err != nil {
				return
			}
			if message.Type == "result" {
				result <- message
				return
			}
		}
	}))
	defer master.Close()

	cfg := testConfig()
	cfg.MasterURL = "ws" + strings.TrimPrefix(master.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	select {
	case message := <-result:
		if message.TaskID != "test-1" || message.Error != nil || message.Response == nil {
			t.Fatalf("unexpected result message: %+v", message)
		}
		if !message.Response.Result.Available || len(message.Response.Result.HTTP) != 1 || message.Response.Result.HTTP[0].StatusCode != http.StatusNoContent {
			t.Fatalf("unexpected probe result: %+v", message.Response.Result)
		}
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Agent result")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Agent did not stop after context cancellation")
	}
}

func TestRunTaskRejectsUnknownType(t *testing.T) {
	_, err := runTask(context.Background(), testConfig(), TaskRequest{TaskID: "bad", Type: "shell", Target: "example.com"})
	if err == nil {
		t.Fatal("expected unsupported task type to be rejected")
	}
}

func TestCertificateTask(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	port := target.Listener.Addr().(*net.TCPAddr).Port
	response, err := runTask(context.Background(), testConfig(), TaskRequest{
		TaskID: "certificate-1",
		Type:   "certificate",
		Target: "127.0.0.1",
		Options: ProbeOptions{
			TimeoutMS: 2000,
			Ports:     []int{port},
		},
	})
	if err != nil {
		t.Fatalf("run certificate task: %v", err)
	}
	if !response.Result.Available || response.Result.Certificate == nil || response.Result.Certificate.ExpiresAt == nil {
		t.Fatalf("unexpected certificate result: %+v", response.Result)
	}
}
