package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxPortsPerTask = 10

type TaskRequest struct {
	TaskID  string       `json:"taskId"`
	Type    string       `json:"type"`
	Target  string       `json:"target"`
	Options ProbeOptions `json:"options"`
}

type ProbeOptions struct {
	TimeoutMS int   `json:"timeoutMs,omitempty"`
	Ports     []int `json:"ports,omitempty"`
}

type TaskResponse struct {
	Version    string      `json:"version"`
	TaskID     string      `json:"taskId"`
	Type       string      `json:"type"`
	Target     string      `json:"target"`
	Agent      AgentInfo   `json:"agent"`
	Status     string      `json:"status"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt time.Time   `json:"finishedAt"`
	DurationMS int64       `json:"durationMs"`
	Result     ProbeResult `json:"result"`
}

type AgentInfo struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
}

type ProbeResult struct {
	NormalizedTarget string             `json:"normalizedTarget"`
	Available        bool               `json:"available"`
	DNS              DNSResult          `json:"dns,omitempty"`
	TCP              []TCPResult        `json:"tcp,omitempty"`
	HTTP             []HTTPResult       `json:"http,omitempty"`
	Certificate      *CertificateResult `json:"certificate,omitempty"`
}

type DNSResult struct {
	Addresses  []string `json:"addresses"`
	DurationMS int64    `json:"durationMs"`
	Error      string   `json:"error,omitempty"`
}

type TCPResult struct {
	Port       int    `json:"port"`
	Reachable  bool   `json:"reachable"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

type HTTPResult struct {
	URL         string `json:"url"`
	StatusCode  int    `json:"statusCode,omitempty"`
	DurationMS  int64  `json:"durationMs"`
	ContentType string `json:"contentType,omitempty"`
	Server      string `json:"server,omitempty"`
	Error       string `json:"error,omitempty"`
}

type targetSpec struct {
	host           string
	normalized     string
	ports          []int
	httpCandidates []string
}

func runTask(parent context.Context, cfg Config, request TaskRequest) (TaskResponse, error) {
	timeout, err := validateTask(cfg, &request)
	if err != nil {
		return TaskResponse{}, err
	}
	switch request.Type {
	case "probe":
		spec, specErr := normalizeTarget(request.Target, request.Options.Ports)
		if specErr != nil {
			return TaskResponse{}, specErr
		}
		request.Target = strings.TrimSpace(request.Target)
		return runProbe(parent, cfg, request, spec, timeout), nil
	case "certificate":
		return runCertificate(parent, cfg, request, timeout)
	default:
		return TaskResponse{}, fmt.Errorf("type must be probe or certificate")
	}
}

func runProbe(parent context.Context, cfg Config, request TaskRequest, spec targetSpec, timeout time.Duration) TaskResponse {

	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result := ProbeResult{NormalizedTarget: spec.normalized}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		result.DNS = probeDNS(ctx, spec.host)
	}()
	go func() {
		defer wg.Done()
		result.TCP = probeTCP(ctx, spec.host, spec.ports)
	}()
	go func() {
		defer wg.Done()
		result.HTTP = probeHTTP(ctx, spec.httpCandidates)
	}()
	wg.Wait()

	for _, item := range result.TCP {
		result.Available = result.Available || item.Reachable
	}
	for _, item := range result.HTTP {
		result.Available = result.Available || (item.StatusCode >= 100 && item.StatusCode < 500)
	}

	finished := time.Now().UTC()
	hostname, _ := osHostname()
	return TaskResponse{
		Version:    "v1",
		TaskID:     request.TaskID,
		Type:       request.Type,
		Target:     request.Target,
		Agent:      AgentInfo{Name: cfg.AgentName, Hostname: hostname},
		Status:     "completed",
		StartedAt:  started,
		FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(),
		Result:     result,
	}
}

var osHostname = os.Hostname

func validateTask(cfg Config, request *TaskRequest) (time.Duration, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	if request.TaskID == "" {
		request.TaskID = newTaskID()
	}
	if len(request.TaskID) > 128 {
		return 0, fmt.Errorf("taskId must be at most 128 characters")
	}
	request.Type = strings.ToLower(strings.TrimSpace(request.Type))
	if request.Type != "probe" && request.Type != "certificate" {
		return 0, fmt.Errorf("type must be probe or certificate")
	}

	timeout := cfg.DefaultTimeout
	if request.Options.TimeoutMS != 0 {
		if request.Options.TimeoutMS < 500 {
			return 0, fmt.Errorf("options.timeoutMs must be at least 500")
		}
		timeout = time.Duration(request.Options.TimeoutMS) * time.Millisecond
	}
	if timeout > cfg.MaxTimeout {
		return 0, fmt.Errorf("options.timeoutMs exceeds the configured maximum")
	}
	return timeout, nil
}

func normalizeTarget(raw string, requestedPorts []int) (targetSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return targetSpec{}, fmt.Errorf("target is required")
	}
	if len(raw) > 2048 {
		return targetSpec{}, fmt.Errorf("target must be at most 2048 characters")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return targetSpec{}, fmt.Errorf("target contains invalid characters")
	}

	explicitScheme := strings.Contains(raw, "://")
	parseValue := raw
	if !explicitScheme {
		if net.ParseIP(raw) != nil && strings.Contains(raw, ":") {
			parseValue = "https://[" + raw + "]"
		} else {
			parseValue = "https://" + raw
		}
	}
	parsed, err := url.ParseRequestURI(parseValue)
	if err != nil || parsed.Hostname() == "" {
		return targetSpec{}, fmt.Errorf("target must be a valid HTTP(S) URL, domain, or IP address")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return targetSpec{}, fmt.Errorf("target scheme must be http or https")
	}
	if parsed.User != nil {
		return targetSpec{}, fmt.Errorf("target must not contain user information")
	}
	if parsed.Fragment != "" {
		parsed.Fragment = ""
	}

	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if host == "" {
		return targetSpec{}, fmt.Errorf("target host is required")
	}
	if parsed.Port() != "" {
		port, portErr := strconv.Atoi(parsed.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return targetSpec{}, fmt.Errorf("target port must be between 1 and 65535")
		}
	}

	ports, err := normalizePorts(requestedPorts)
	if err != nil {
		return targetSpec{}, err
	}
	if len(ports) == 0 {
		if parsed.Port() != "" {
			port, _ := strconv.Atoi(parsed.Port())
			ports = []int{port}
		} else if explicitScheme && parsed.Scheme == "http" {
			ports = []int{80}
		} else if explicitScheme {
			ports = []int{443}
		} else {
			ports = []int{80, 443}
		}
	}

	candidates := []string{parsed.String()}
	if !explicitScheme {
		httpURL := *parsed
		httpURL.Scheme = "http"
		candidates = append(candidates, httpURL.String())
	}
	return targetSpec{host: host, normalized: parsed.String(), ports: ports, httpCandidates: candidates}, nil
}

func normalizePorts(ports []int) ([]int, error) {
	if len(ports) > maxPortsPerTask {
		return nil, fmt.Errorf("options.ports supports at most %d ports", maxPortsPerTask)
	}
	seen := make(map[int]struct{}, len(ports))
	normalized := make([]int, 0, len(ports))
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("options.ports values must be between 1 and 65535")
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		normalized = append(normalized, port)
	}
	sort.Ints(normalized)
	return normalized, nil
}

func probeDNS(ctx context.Context, host string) DNSResult {
	started := time.Now()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	result := DNSResult{DurationMS: time.Since(started).Milliseconds(), Addresses: []string{}}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		value := address.IP.String()
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result.Addresses = append(result.Addresses, value)
		}
	}
	sort.Strings(result.Addresses)
	return result
}

func probeTCP(ctx context.Context, host string, ports []int) []TCPResult {
	results := make([]TCPResult, len(ports))
	var wg sync.WaitGroup
	for index, port := range ports {
		wg.Add(1)
		go func(index, port int) {
			defer wg.Done()
			started := time.Now()
			connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
			result := TCPResult{Port: port, DurationMS: time.Since(started).Milliseconds()}
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Reachable = true
				_ = connection.Close()
			}
			results[index] = result
		}(index, port)
	}
	wg.Wait()
	return results
}

func probeHTTP(ctx context.Context, candidates []string) []HTTPResult {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        2,
		IdleConnTimeout:     10 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return nil
		},
	}

	results := make([]HTTPResult, 0, len(candidates))
	for _, candidate := range candidates {
		started := time.Now()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate, nil)
		if err != nil {
			results = append(results, HTTPResult{URL: candidate, DurationMS: time.Since(started).Milliseconds(), Error: err.Error()})
			continue
		}
		request.Header.Set("User-Agent", "SituationAwareness-Agent/1.0")
		request.Header.Set("Range", "bytes=0-1023")
		response, err := client.Do(request)
		item := HTTPResult{URL: candidate, DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			item.Error = err.Error()
			results = append(results, item)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		_ = response.Body.Close()
		item.StatusCode = response.StatusCode
		item.ContentType = response.Header.Get("Content-Type")
		item.Server = response.Header.Get("Server")
		results = append(results, item)
		break
	}
	return results
}

func newTaskID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(value[:])
}
