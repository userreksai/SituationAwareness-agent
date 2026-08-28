package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const maxTitleRunes = 4096

type TitleResult struct {
	Title       string    `json:"title,omitempty"`
	FinalURL    string    `json:"finalUrl,omitempty"`
	StatusCode  int       `json:"statusCode,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	CheckedAt   time.Time `json:"checkedAt"`
	Error       string    `json:"error,omitempty"`
}

func runTitle(parent context.Context, cfg Config, request TaskRequest, timeout time.Duration) (TaskResponse, error) {
	spec, err := normalizeTarget(request.Target, nil)
	if err != nil {
		return TaskResponse{}, err
	}
	request.Target = strings.TrimSpace(request.Target)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result := fetchTitle(ctx, cfg, spec.httpCandidates)
	finished := time.Now().UTC()
	hostname, _ := os.Hostname()
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
		Result: ProbeResult{
			NormalizedTarget: spec.normalized,
			Available:        result.Error == "",
			Title:            &result,
		},
	}, nil
}

func fetchTitle(ctx context.Context, cfg Config, candidates []string) TitleResult {
	checkedAt := time.Now().UTC()
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        2,
		IdleConnTimeout:     10 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
				return errors.New("redirect target must use http or https")
			}
			if request.URL.User != nil {
				return errors.New("redirect target must not contain user information")
			}
			return nil
		},
	}

	failures := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result, err := fetchTitleURL(ctx, client, candidate, cfg.TitleMaxResponseBytes, checkedAt)
		if err == nil {
			return result
		}
		failures = append(failures, candidate+": "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	message := strings.Join(failures, "; ")
	if message == "" {
		message = "title request failed"
	}
	return TitleResult{CheckedAt: checkedAt, Error: message}
}

func fetchTitleURL(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time) (TitleResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return TitleResult{}, err
	}
	request.Header.Set("User-Agent", "SituationAwareness-Agent/1.0")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")

	response, err := client.Do(request)
	if err != nil {
		return TitleResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return TitleResult{}, fmt.Errorf("target returned HTTP %d", response.StatusCode)
	}

	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return TitleResult{}, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return TitleResult{}, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	title, err := extractTitle(body, response.Header.Get("Content-Type"))
	if err != nil {
		return TitleResult{}, err
	}
	if utf8.RuneCountInString(title) > maxTitleRunes {
		return TitleResult{}, fmt.Errorf("title exceeds %d characters", maxTitleRunes)
	}
	finalURL := target
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	return TitleResult{
		Title:       title,
		FinalURL:    finalURL,
		StatusCode:  response.StatusCode,
		ContentType: response.Header.Get("Content-Type"),
		CheckedAt:   checkedAt,
	}, nil
}

func extractTitle(body []byte, contentType string) (string, error) {
	var reader io.Reader = bytes.NewReader(body)
	if !utf8.Valid(body) {
		decoded, err := charset.NewReader(reader, contentType)
		if err != nil {
			return "", fmt.Errorf("detect HTML encoding: %w", err)
		}
		reader = decoded
	}
	tokenizer := html.NewTokenizer(reader)
	inTitle := false
	var value strings.Builder
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if err := tokenizer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("parse HTML: %w", err)
			}
			title := strings.Join(strings.Fields(value.String()), " ")
			if title == "" {
				return "", errors.New("page has no valid title element")
			}
			return title, nil
		case html.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "title") {
				inTitle = true
			}
		case html.TextToken:
			if inTitle {
				value.Write(tokenizer.Text())
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			if inTitle && strings.EqualFold(token.Data, "title") {
				title := strings.Join(strings.Fields(value.String()), " ")
				if title == "" {
					return "", errors.New("page title is empty")
				}
				return title, nil
			}
		}
	}
}
