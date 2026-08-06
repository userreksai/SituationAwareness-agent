package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	agentProtocolVersion = "v1"
	maxControlMessage    = 2 << 20
	writeTimeout         = 10 * time.Second
)

type controlMessage struct {
	Type             string            `json:"type"`
	Version          string            `json:"version,omitempty"`
	AgentID          string            `json:"agentId,omitempty"`
	AgentName        string            `json:"agentName,omitempty"`
	HeartbeatSeconds int               `json:"heartbeatSeconds,omitempty"`
	TaskID           string            `json:"taskId,omitempty"`
	Task             *TaskRequest      `json:"task,omitempty"`
	Response         *TaskResponse     `json:"response,omitempty"`
	Error            *controlTaskError `json:"error,omitempty"`
	Time             time.Time         `json:"time,omitempty"`
}

type controlTaskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Run keeps a single outbound control connection to Master until the context is cancelled.
func Run(ctx context.Context, cfg Config, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{
		Proxy:            nil,
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  tlsConfig,
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		EnableCompression: false,
	}

	backoff := cfg.ReconnectMin
	for ctx.Err() == nil {
		connected, connectErr := connect(ctx, cfg, &dialer, logger)
		if ctx.Err() != nil {
			return nil
		}
		if connectErr != nil {
			logger.Printf("master connection ended: %v", connectErr)
		}
		if connected {
			backoff = cfg.ReconnectMin
		} else if backoff < cfg.ReconnectMax {
			backoff *= 2
			if backoff > cfg.ReconnectMax {
				backoff = cfg.ReconnectMax
			}
		}

		wait := backoff
		if spread := backoff / 4; spread > 0 {
			wait += time.Duration(rand.Int63n(int64(spread)))
		}
		logger.Printf("reconnecting to Master in %s", wait.Round(time.Millisecond))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func connect(ctx context.Context, cfg Config, dialer *websocket.Dialer, logger *log.Logger) (bool, error) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+cfg.SharedToken)
	headers.Set("X-Agent-Name", cfg.AgentName)
	headers.Set("X-Agent-Version", agentProtocolVersion)
	connection, response, err := dialer.DialContext(ctx, cfg.MasterURL, headers)
	if err != nil {
		if response == nil {
			return false, fmt.Errorf("connect %s: %w", cfg.MasterURL, err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return false, fmt.Errorf("Master rejected connection with HTTP %d: %s", response.StatusCode, message)
	}
	defer connection.Close()
	logger.Printf("connected to Master at %s", cfg.MasterURL)
	return true, serveConnection(ctx, cfg, connection, logger)
}

func serveConnection(parent context.Context, cfg Config, connection *websocket.Conn, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connection.SetReadLimit(maxControlMessage)

	outbound := make(chan controlMessage, max(16, cfg.MaxConcurrent*2))
	semaphore := make(chan struct{}, cfg.MaxConcurrent)
	errors := make(chan error, 2)

	go func() { errors <- writeControlLoop(ctx, cfg, connection, outbound) }()
	go func() { errors <- readControlLoop(ctx, cfg, connection, outbound, semaphore, logger) }()

	select {
	case <-parent.Done():
		cancel()
		_ = connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "agent shutting down"),
			time.Now().Add(time.Second),
		)
		_ = connection.Close()
		return nil
	case err := <-errors:
		cancel()
		_ = connection.Close()
		return err
	}
}

func readControlLoop(ctx context.Context, cfg Config, connection *websocket.Conn, outbound chan<- controlMessage, semaphore chan struct{}, logger *log.Logger) error {
	for {
		var message controlMessage
		if err := connection.ReadJSON(&message); err != nil {
			return fmt.Errorf("read Master message: %w", err)
		}
		switch message.Type {
		case "registered":
			logger.Printf("registered by Master as agent_id=%s", message.AgentID)
		case "task":
			if message.Task == nil {
				return fmt.Errorf("Master sent task message without a task")
			}
			task := *message.Task
			select {
			case semaphore <- struct{}{}:
				go func() {
					defer func() { <-semaphore }()
					runAndReportTask(ctx, cfg, task, outbound, logger)
				}()
			default:
				sendControl(ctx, outbound, controlMessage{
					Type:   "result",
					TaskID: task.TaskID,
					Error:  &controlTaskError{Code: "agent_busy", Message: "agent has reached its concurrent task limit"},
				})
			}
		default:
			return fmt.Errorf("Master sent unsupported message type %q", message.Type)
		}
	}
}

func runAndReportTask(ctx context.Context, cfg Config, task TaskRequest, outbound chan<- controlMessage, logger *log.Logger) {
	response, err := runTask(ctx, cfg, task)
	message := controlMessage{Type: "result", TaskID: task.TaskID}
	if err != nil {
		message.Error = &controlTaskError{Code: "invalid_task", Message: err.Error()}
	} else {
		message.Response = &response
		logger.Printf("task=%s target=%q available=%t duration_ms=%d", response.TaskID, response.Target, response.Result.Available, response.DurationMS)
	}
	sendControl(ctx, outbound, message)
}

func sendControl(ctx context.Context, outbound chan<- controlMessage, message controlMessage) {
	select {
	case outbound <- message:
	case <-ctx.Done():
	}
}

func writeControlLoop(ctx context.Context, cfg Config, connection *websocket.Conn, outbound <-chan controlMessage) error {
	ticker := time.NewTicker(cfg.Heartbeat)
	defer ticker.Stop()
	for {
		var message controlMessage
		select {
		case <-ctx.Done():
			return nil
		case message = <-outbound:
		case now := <-ticker.C:
			message = controlMessage{Type: "heartbeat", Version: agentProtocolVersion, AgentName: cfg.AgentName, Time: now.UTC()}
		}
		if err := connection.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return fmt.Errorf("set Master write deadline: %w", err)
		}
		if err := connection.WriteJSON(message); err != nil {
			return fmt.Errorf("write Master message: %w", err)
		}
	}
}
