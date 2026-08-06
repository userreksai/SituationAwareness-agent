package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MasterURL      string
	AgentName      string
	SharedToken    string
	MaxConcurrent  int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	Heartbeat      time.Duration
	TLSCAFile      string
	TLSServerName  string
}

func LoadConfig() (Config, error) {
	hostname, _ := os.Hostname()
	cfg := Config{
		MasterURL:      envOrDefault("AGENT_MASTER_URL", "ws://127.0.0.1:9910/api/v1/agent/connect"),
		AgentName:      envOrDefault("AGENT_NAME", hostname),
		SharedToken:    strings.TrimSpace(os.Getenv("AGENT_SHARED_TOKEN")),
		MaxConcurrent:  8,
		DefaultTimeout: 10 * time.Second,
		MaxTimeout:     30 * time.Second,
		ReconnectMin:   time.Second,
		ReconnectMax:   30 * time.Second,
		Heartbeat:      20 * time.Second,
		TLSCAFile:      strings.TrimSpace(os.Getenv("AGENT_TLS_CA_FILE")),
		TLSServerName:  strings.TrimSpace(os.Getenv("AGENT_TLS_SERVER_NAME")),
	}

	var err error
	if cfg.MaxConcurrent, err = positiveIntEnv("AGENT_MAX_CONCURRENT", cfg.MaxConcurrent); err != nil {
		return Config{}, err
	}
	if cfg.DefaultTimeout, err = durationEnv("AGENT_DEFAULT_TIMEOUT", cfg.DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxTimeout, err = durationEnv("AGENT_MAX_TIMEOUT", cfg.MaxTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ReconnectMin, err = durationEnv("AGENT_RECONNECT_MIN", cfg.ReconnectMin); err != nil {
		return Config{}, err
	}
	if cfg.ReconnectMax, err = durationEnv("AGENT_RECONNECT_MAX", cfg.ReconnectMax); err != nil {
		return Config{}, err
	}
	if cfg.Heartbeat, err = durationEnv("AGENT_HEARTBEAT_INTERVAL", cfg.Heartbeat); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return Config{}, fmt.Errorf("AGENT_NAME cannot be empty")
	}
	if len(cfg.AgentName) > 128 || strings.ContainsAny(cfg.AgentName, "\r\n\x00") {
		return Config{}, fmt.Errorf("AGENT_NAME must be at most 128 characters and contain no control characters")
	}
	if cfg.SharedToken == "" {
		return Config{}, fmt.Errorf("AGENT_SHARED_TOKEN cannot be empty")
	}
	if len(cfg.SharedToken) < 32 {
		return Config{}, fmt.Errorf("AGENT_SHARED_TOKEN must contain at least 32 characters")
	}
	if len(cfg.SharedToken) > 512 || strings.IndexFunc(cfg.SharedToken, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return Config{}, fmt.Errorf("AGENT_SHARED_TOKEN must be at most 512 characters and contain no whitespace or control characters")
	}
	if cfg.DefaultTimeout > cfg.MaxTimeout {
		return Config{}, fmt.Errorf("AGENT_DEFAULT_TIMEOUT cannot exceed AGENT_MAX_TIMEOUT")
	}
	if cfg.ReconnectMin > cfg.ReconnectMax {
		return Config{}, fmt.Errorf("AGENT_RECONNECT_MIN cannot exceed AGENT_RECONNECT_MAX")
	}
	parsedMasterURL, err := url.ParseRequestURI(cfg.MasterURL)
	if err != nil || parsedMasterURL.Host == "" || (parsedMasterURL.Scheme != "ws" && parsedMasterURL.Scheme != "wss") {
		return Config{}, fmt.Errorf("AGENT_MASTER_URL must be a valid ws:// or wss:// URL")
	}
	if parsedMasterURL.User != nil {
		return Config{}, fmt.Errorf("AGENT_MASTER_URL must not contain credentials")
	}
	if parsedMasterURL.RawQuery != "" || parsedMasterURL.Fragment != "" {
		return Config{}, fmt.Errorf("AGENT_MASTER_URL must not contain query parameters or fragments")
	}
	if cfg.TLSCAFile != "" && parsedMasterURL.Scheme != "wss" {
		return Config{}, fmt.Errorf("AGENT_TLS_CA_FILE requires a wss:// AGENT_MASTER_URL")
	}
	return cfg, nil
}

func (cfg Config) TLSConfig() (*tls.Config, error) {
	if cfg.TLSCAFile == "" && cfg.TLSServerName == "" {
		return nil, nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.TLSServerName}
	if cfg.TLSCAFile == "" {
		return config, nil
	}
	pem, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read AGENT_TLS_CA_FILE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("AGENT_TLS_CA_FILE contains no valid certificates")
	}
	config.RootCAs = pool
	return config, nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func positiveIntEnv(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}
