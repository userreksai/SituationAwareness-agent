package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type handler struct {
	cfg       Config
	logger    *log.Logger
	semaphore chan struct{}
}

func NewHandler(cfg Config, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	service := &handler{cfg: cfg, logger: logger, semaphore: make(chan struct{}, cfg.MaxConcurrent)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", service.health)
	mux.HandleFunc("GET /api/v1/health", service.health)
	mux.HandleFunc("POST /api/v1/tasks", service.tasks)
	return service.securityHeaders(mux)
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"service":       "situation-awareness-agent",
		"agentName":     h.cfg.AgentName,
		"authenticated": h.cfg.SharedToken != "",
		"time":          time.Now().UTC(),
	})
}

func (h *handler) tasks(w http.ResponseWriter, request *http.Request) {
	if !h.authorized(request) {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	default:
		writeAPIError(w, http.StatusTooManyRequests, "agent_busy", "agent has reached its concurrent task limit")
		return
	}

	request.Body = http.MaxBytesReader(w, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var task TaskRequest
	if err := decoder.Decode(&task); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", jsonErrorMessage(err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return
	}

	response, err := runProbe(request.Context(), h.cfg, task)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_task", err.Error())
		return
	}
	h.logger.Printf("task=%s target=%q available=%t duration_ms=%d", response.TaskID, response.Target, response.Result.Available, response.DurationMS)
	writeJSON(w, http.StatusOK, response)
}

func (h *handler) authorized(request *http.Request) bool {
	if h.cfg.SharedToken == "" {
		return true
	}
	provided := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.SharedToken)) == 1
}

func (h *handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, request)
	})
}

func jsonErrorMessage(err error) string {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return "request body is too large"
	}
	return fmt.Sprintf("invalid JSON: %v", err)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
