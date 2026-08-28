package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		ListenAddr:            ":8002",
		AgentName:             "test-agent",
		SharedToken:           "test-token",
		MaxConcurrent:         2,
		DefaultTimeout:        2 * time.Second,
		MaxTimeout:            5 * time.Second,
		TitleMaxResponseBytes: 2 * 1024 * 1024,
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

func TestTaskHandlerAuthenticatesAndProbes(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	server := httptest.NewServer(NewHandler(testConfig(), log.New(io.Discard, "", 0)))
	defer server.Close()

	payload, _ := json.Marshal(TaskRequest{TaskID: "test-1", Type: "probe", Target: target.URL, Options: ProbeOptions{TimeoutMS: 2000}})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("unauthenticated request failed: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("probe request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("probe status=%d body=%s", response.StatusCode, body)
	}
	var result TaskResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.TaskID != "test-1" || !result.Result.Available {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Result.HTTP) != 1 || result.Result.HTTP[0].StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected HTTP result: %+v", result.Result.HTTP)
	}
}

func TestTaskHandlerRejectsUnknownParameters(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(NewHandler(testConfig(), log.New(io.Discard, "", 0)))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", bytes.NewBufferString(`{"type":"probe","target":"example.com","command":"whoami"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestCertificateTaskHandler(t *testing.T) {
	t.Parallel()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	port := target.Listener.Addr().(*net.TCPAddr).Port

	server := httptest.NewServer(NewHandler(testConfig(), log.New(io.Discard, "", 0)))
	defer server.Close()
	payload, _ := json.Marshal(TaskRequest{
		TaskID: "certificate-1",
		Type:   "certificate",
		Target: "127.0.0.1",
		Options: ProbeOptions{
			TimeoutMS: 2000,
			Ports:     []int{port},
		},
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("certificate request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("certificate status=%d body=%s", response.StatusCode, body)
	}
	var result TaskResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode certificate response: %v", err)
	}
	if !result.Result.Available || result.Result.Certificate == nil {
		t.Fatalf("unexpected certificate result: %+v", result.Result)
	}
	if result.Result.Certificate.ExpiresAt == nil || result.Result.Certificate.ResolvedAddress == "" {
		t.Fatalf("missing certificate details: %+v", result.Result.Certificate)
	}
}

func TestTitleTaskHandler(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title> SEO综合查询  -  站长工具 </title></head></html>"))
	}))
	defer target.Close()

	server := httptest.NewServer(NewHandler(testConfig(), log.New(io.Discard, "", 0)))
	defer server.Close()
	payload, _ := json.Marshal(TaskRequest{
		TaskID: "title-1", Type: "title", Target: target.URL,
		Options: ProbeOptions{TimeoutMS: 2000},
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("title request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("title status=%d body=%s", response.StatusCode, body)
	}
	var result TaskResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode title response: %v", err)
	}
	if !result.Result.Available || result.Result.Title == nil {
		t.Fatalf("unexpected title result: %+v", result.Result)
	}
	if result.Result.Title.Title != "SEO综合查询 - 站长工具" || result.Result.Title.StatusCode != http.StatusOK {
		t.Fatalf("unexpected title details: %+v", result.Result.Title)
	}
}

func TestExtractTitleDecodesGBK(t *testing.T) {
	body := append([]byte("<html><head><meta charset=gbk><title>"),
		[]byte{0xd6, 0xd0, 0xce, 0xc4, 0xb1, 0xea, 0xcc, 0xe2}...)
	body = append(body, []byte("</title></head></html>")...)
	title, err := extractTitle(body, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != "中文标题" {
		t.Fatalf("title = %q", title)
	}
}
