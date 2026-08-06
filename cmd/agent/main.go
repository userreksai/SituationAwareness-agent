package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/userreksai/SituationAwareness-agent/internal/agent"
)

func main() {
	logger := log.New(os.Stdout, "agent ", log.LstdFlags|log.LUTC)
	cfg, err := agent.LoadConfig()
	if err != nil {
		logger.Fatalf("invalid configuration: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Printf("starting as %s, master=%s, inbound_listener=disabled", cfg.AgentName, cfg.MasterURL)
	if err := agent.Run(ctx, cfg, logger); err != nil {
		logger.Fatalf("agent stopped: %v", err)
	}
	logger.Printf("shutdown complete")
}
