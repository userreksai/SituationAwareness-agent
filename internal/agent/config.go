package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	AgentName      string
	SharedToken    string
	MaxConcurrent  int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

func LoadConfig() (Config, error) {
	hostname, _ := os.Hostname()
	cfg := Config{
		ListenAddr:     envOrDefault("AGENT_LISTEN_ADDR", ":8002"),
		AgentName:      envOrDefault("AGENT_NAME", hostname),
		SharedToken:    strings.TrimSpace(os.Getenv("AGENT_SHARED_TOKEN")),
		MaxConcurrent:  8,
		DefaultTimeout: 10 * time.Second,
		MaxTimeout:     30 * time.Second,
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
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return Config{}, fmt.Errorf("AGENT_LISTEN_ADDR cannot be empty")
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return Config{}, fmt.Errorf("AGENT_NAME cannot be empty")
	}
	if cfg.DefaultTimeout > cfg.MaxTimeout {
		return Config{}, fmt.Errorf("AGENT_DEFAULT_TIMEOUT cannot exceed AGENT_MAX_TIMEOUT")
	}
	return cfg, nil
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
