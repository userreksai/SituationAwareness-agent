package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/encoding"
)

const (
	maxTitleRunes            = 4096
	maxFrameDepth            = 2
	maxFrameCandidates       = 8
	maxCharsetScanBytes      = 64 * 1024
	maxSoftErrorTitleRunes   = 128
	maxDiagnosticTextRunes   = 512
	monitoringTitleUserAgent = "SituationAwareness-Agent/1.0"
	browserTitleUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var genericPageTitles = map[string]struct{}{
	"apache http server test page powered by centos": {},
	"apache2 debian default page: it works":          {},
	"apache2 ubuntu default page: it works":          {},
	"iis windows server":                             {},
	"index of /":                                     {},
	"internet information services":                  {},
	"test page for the nginx http server on fedora":  {},
	"welcome to nginx":                               {},
	"welcome to nginx!":                              {},
}

type TitleResult struct {
	Title       string         `json:"title,omitempty"`
	FinalURL    string         `json:"finalUrl,omitempty"`
	StatusCode  int            `json:"statusCode,omitempty"`
	ContentType string         `json:"contentType,omitempty"`
	Server      string         `json:"server,omitempty"`
	CheckedAt   time.Time      `json:"checkedAt"`
	Error       string         `json:"error,omitempty"`
	Attempts    []TitleAttempt `json:"attempts,omitempty"`
}

type TitleAttempt struct {
	RequestedURL string `json:"requestedUrl"`
	FinalURL     string `json:"finalUrl,omitempty"`
	StatusCode   int    `json:"statusCode,omitempty"`
	Title        string `json:"title,omitempty"`
	ContentType  string `json:"contentType,omitempty"`
	Server       string `json:"server,omitempty"`
	UserAgent    string `json:"userAgent"`
	BrowserRetry bool   `json:"browserRetry,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

type titleRequestProfile struct {
	userAgent    string
	browserRetry bool
}

var (
	monitoringTitleProfile = titleRequestProfile{userAgent: monitoringTitleUserAgent}
	browserTitleProfile    = titleRequestProfile{userAgent: browserTitleUserAgent, browserRetry: true}
)

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

	titleCandidates := expandTitleCandidates(candidates)
	failures := make([]string, 0, len(titleCandidates)*2)
	attempts := make([]TitleAttempt, 0, len(titleCandidates)*2)
	for _, candidate := range titleCandidates {
		result, err := fetchTitleURLWithProfile(ctx, client, candidate, cfg.TitleMaxResponseBytes, checkedAt, monitoringTitleProfile)
		if err != nil {
			attempts = append(attempts, newTitleAttempt(candidate, result, monitoringTitleProfile, "error", "", err))
			failures = append(failures, truncateRunes(candidate+": "+err.Error(), maxDiagnosticTextRunes))
			if ctx.Err() != nil {
				break
			}
			continue
		}

		if reason := softErrorTitleReason(result.Title); reason != "" {
			attempts = append(attempts, newTitleAttempt(candidate, result, monitoringTitleProfile, "soft_404", reason, nil))
			failures = append(failures, rejectedTitleFailure(candidate, result, "soft 404", reason))

			retryTarget := result.FinalURL
			if retryTarget == "" {
				retryTarget = candidate
			}
			browserResult, browserErr := fetchTitleURLWithProfile(ctx, client, retryTarget, cfg.TitleMaxResponseBytes, checkedAt, browserTitleProfile)
			if browserErr != nil {
				attempts = append(attempts, newTitleAttempt(retryTarget, browserResult, browserTitleProfile, "error", "", browserErr))
				failures = append(failures, truncateRunes(retryTarget+" (browser retry): "+browserErr.Error(), maxDiagnosticTextRunes))
				if ctx.Err() != nil {
					break
				}
				continue
			}
			if browserReason := softErrorTitleReason(browserResult.Title); browserReason != "" {
				attempts = append(attempts, newTitleAttempt(retryTarget, browserResult, browserTitleProfile, "soft_404", browserReason, nil))
				failures = append(failures, rejectedTitleFailure(retryTarget+" (browser retry)", browserResult, "soft 404", browserReason))
				continue
			}
			if isGenericPageTitle(browserResult.Title) {
				reason := "known server default title"
				attempts = append(attempts, newTitleAttempt(retryTarget, browserResult, browserTitleProfile, "generic_title", reason, nil))
				failures = append(failures, rejectedTitleFailure(retryTarget+" (browser retry)", browserResult, "generic title", reason))
				continue
			}
			attempts = append(attempts, newTitleAttempt(retryTarget, browserResult, browserTitleProfile, "success", "", nil))
			browserResult.Attempts = attempts
			return browserResult
		}

		if isGenericPageTitle(result.Title) {
			reason := "known server default title"
			attempts = append(attempts, newTitleAttempt(candidate, result, monitoringTitleProfile, "generic_title", reason, nil))
			failures = append(failures, rejectedTitleFailure(candidate, result, "generic title", reason))
			continue
		}

		attempts = append(attempts, newTitleAttempt(candidate, result, monitoringTitleProfile, "success", "", nil))
		result.Attempts = attempts
		return result
	}
	message := strings.Join(failures, "; ")
	if message == "" {
		message = "title request failed"
	}
	return TitleResult{CheckedAt: checkedAt, Error: message, Attempts: attempts}
}

func fetchTitleURL(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time) (TitleResult, error) {
	return fetchTitleURLWithProfile(ctx, client, target, maxResponseBytes, checkedAt, monitoringTitleProfile)
}

func fetchTitleURLWithProfile(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time, profile titleRequestProfile) (TitleResult, error) {
	return fetchTitleURLDepth(ctx, client, target, maxResponseBytes, checkedAt, make(map[string]struct{}), 0, profile)
}

func fetchTitleURLDepth(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time, visited map[string]struct{}, depth int, profile titleRequestProfile) (TitleResult, error) {
	if _, ok := visited[target]; ok {
		return TitleResult{FinalURL: target, CheckedAt: checkedAt}, errors.New("frame URL loop detected")
	}
	visited[target] = struct{}{}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return TitleResult{FinalURL: target, CheckedAt: checkedAt}, err
	}
	request.Header.Set("User-Agent", profile.userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")
	if profile.browserRetry {
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		request.Header.Set("Cache-Control", "no-cache")
		request.Header.Set("Pragma", "no-cache")
		request.Header.Set("Upgrade-Insecure-Requests", "1")
	}

	response, err := client.Do(request)
	if err != nil {
		return TitleResult{FinalURL: target, CheckedAt: checkedAt}, err
	}
	defer response.Body.Close()
	finalURL := target
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	result := TitleResult{
		FinalURL:    finalURL,
		StatusCode:  response.StatusCode,
		ContentType: truncateRunes(response.Header.Get("Content-Type"), maxDiagnosticTextRunes),
		Server:      truncateRunes(strings.TrimSpace(response.Header.Get("Server")), maxDiagnosticTextRunes),
		CheckedAt:   checkedAt,
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return result, fmt.Errorf("target returned HTTP %d", response.StatusCode)
	}

	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return result, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return result, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	document, err := parseTitleDocument(body, response.Header.Get("Content-Type"))
	if err != nil {
		return result, err
	}
	if utf8.RuneCountInString(document.title) > maxTitleRunes {
		return result, fmt.Errorf("title exceeds %d characters", maxTitleRunes)
	}
	result.Title = document.title

	if depth < maxFrameDepth && (result.Title == "" || isGenericPageTitle(result.Title)) {
		frameFailures := make([]string, 0, len(document.frameSources))
		for _, source := range document.frameSources {
			frameURL, resolveErr := resolveSameSiteFrameURL(finalURL, source)
			if resolveErr != nil {
				frameFailures = append(frameFailures, source+": "+resolveErr.Error())
				continue
			}
			frameResult, frameErr := fetchTitleURLDepth(ctx, client, frameURL, maxResponseBytes, checkedAt, visited, depth+1, profile)
			if frameErr != nil {
				frameFailures = append(frameFailures, frameURL+": "+frameErr.Error())
				continue
			}
			if !isGenericPageTitle(frameResult.Title) {
				return frameResult, nil
			}
		}
		if result.Title == "" && len(frameFailures) > 0 {
			return result, fmt.Errorf("page title is empty; frame lookup failed: %s", strings.Join(frameFailures, "; "))
		}
	}
	if result.Title == "" {
		return result, errors.New("page has no valid title element")
	}
	return result, nil
}

func extractTitle(body []byte, contentType string) (string, error) {
	document, err := parseTitleDocument(body, contentType)
	if err != nil {
		return "", err
	}
	if document.title == "" {
		return "", errors.New("page has no valid title element")
	}
	return document.title, nil
}

type titleDocument struct {
	title        string
	frameSources []string
}

func parseTitleDocument(body []byte, contentType string) (titleDocument, error) {
	detectedEncoding, _, certain := charset.DetermineEncoding(body, contentType)
	var reader io.Reader = bytes.NewReader(body)
	if certain {
		reader = detectedEncoding.NewDecoder().Reader(reader)
	} else if declaredEncoding, ok := findDeclaredHTMLEncoding(body); ok {
		reader = declaredEncoding.NewDecoder().Reader(reader)
	} else if !utf8.Valid(body) {
		reader = detectedEncoding.NewDecoder().Reader(reader)
	}
	tokenizer := html.NewTokenizer(reader)
	inTitle := false
	var value strings.Builder
	document := titleDocument{frameSources: make([]string, 0, 1)}
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if err := tokenizer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return titleDocument{}, fmt.Errorf("parse HTML: %w", err)
			}
			if document.title == "" {
				document.title = normalizeTitle(value.String())
			}
			return document, nil
		case html.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "title") {
				inTitle = true
				value.Reset()
			}
			if (strings.EqualFold(token.Data, "frame") || strings.EqualFold(token.Data, "iframe")) && len(document.frameSources) < maxFrameCandidates {
				for _, attribute := range token.Attr {
					if strings.EqualFold(attribute.Key, "src") {
						source := strings.TrimSpace(attribute.Val)
						if source != "" {
							document.frameSources = append(document.frameSources, source)
						}
						break
					}
				}
			}
		case html.TextToken:
			if inTitle {
				value.Write(tokenizer.Text())
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			if inTitle && strings.EqualFold(token.Data, "title") {
				document.title = normalizeTitle(value.String())
				inTitle = false
				if document.title == "" {
					value.Reset()
				}
			}
		}
	}
}

func findDeclaredHTMLEncoding(body []byte) (encoding.Encoding, bool) {
	if len(body) > maxCharsetScanBytes {
		body = body[:maxCharsetScanBytes]
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return nil, false
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if !strings.EqualFold(token.Data, "meta") {
				continue
			}
			attributes := make(map[string]string, len(token.Attr))
			for _, attribute := range token.Attr {
				attributes[strings.ToLower(attribute.Key)] = strings.TrimSpace(attribute.Val)
			}
			if label := attributes["charset"]; label != "" {
				if declaredEncoding, _ := charset.Lookup(label); declaredEncoding != nil {
					return declaredEncoding, true
				}
			}
			if !strings.EqualFold(attributes["http-equiv"], "content-type") {
				continue
			}
			_, parameters, err := mime.ParseMediaType(attributes["content"])
			if err != nil {
				continue
			}
			if declaredEncoding, _ := charset.Lookup(parameters["charset"]); declaredEncoding != nil {
				return declaredEncoding, true
			}
		}
	}
}

func normalizeTitle(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func isGenericPageTitle(title string) bool {
	normalized := strings.ToLower(normalizeTitle(title))
	_, ok := genericPageTitles[normalized]
	return ok
}

func softErrorTitleReason(title string) string {
	normalized := strings.ToLower(normalizeTitle(title))
	if normalized == "" || utf8.RuneCountInString(normalized) > maxSoftErrorTitleRunes {
		return ""
	}
	trimmed := strings.Trim(normalized, " \t\r\n-_|:：—–·()[]【】")
	runes := []rune(trimmed)
	if len(runes) >= 3 && runes[len(runes)-3] == '4' && runes[len(runes)-2] == '0' && runes[len(runes)-1] == '4' {
		if len(runes) == 3 || !unicode.IsDigit(runes[len(runes)-4]) {
			return "title is or ends with 404"
		}
	}

	for _, phrase := range []string{
		"访问的页面不存在",
		"页面不存在",
		"找不到页面",
		"网页不存在",
		"页面未找到",
	} {
		if hasStandaloneTitleMarker(normalized, phrase) {
			return "title indicates " + phrase
		}
	}
	if strings.Contains(normalized, "404 not found") {
		return "title contains 404 not found"
	}
	for _, phrase := range []string{"page not found", "not found"} {
		if hasStandaloneTitleMarker(normalized, phrase) {
			return "title indicates " + phrase
		}
	}
	return ""
}

func hasStandaloneTitleMarker(title, marker string) bool {
	if title == marker {
		return true
	}
	for _, separator := range []string{" - ", "-", " | ", "|", " — ", "—", " – ", "–", "_", ":", "："} {
		if strings.HasPrefix(title, marker+separator) || strings.HasSuffix(title, separator+marker) {
			return true
		}
	}
	return false
}

func newTitleAttempt(requestedURL string, result TitleResult, profile titleRequestProfile, outcome, reason string, err error) TitleAttempt {
	finalURL := result.FinalURL
	if finalURL == "" {
		finalURL = requestedURL
	}
	attempt := TitleAttempt{
		RequestedURL: truncateRunes(requestedURL, maxDiagnosticTextRunes),
		FinalURL:     truncateRunes(finalURL, maxDiagnosticTextRunes),
		StatusCode:   result.StatusCode,
		Title:        truncateRunes(result.Title, maxDiagnosticTextRunes),
		ContentType:  truncateRunes(result.ContentType, maxDiagnosticTextRunes),
		Server:       truncateRunes(result.Server, maxDiagnosticTextRunes),
		UserAgent:    profile.userAgent,
		BrowserRetry: profile.browserRetry,
		Outcome:      outcome,
		Reason:       truncateRunes(reason, maxDiagnosticTextRunes),
	}
	if err != nil {
		attempt.Error = truncateRunes(err.Error(), maxDiagnosticTextRunes)
	}
	return attempt
}

func rejectedTitleFailure(requestedURL string, result TitleResult, category, reason string) string {
	finalURL := result.FinalURL
	if finalURL == "" {
		finalURL = requestedURL
	}
	return fmt.Sprintf(
		"%s: %s %q (%s; final URL %s)",
		truncateRunes(requestedURL, maxDiagnosticTextRunes),
		category,
		truncateRunes(result.Title, maxDiagnosticTextRunes),
		reason,
		truncateRunes(finalURL, maxDiagnosticTextRunes),
	)
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum]) + "…"
}

func expandTitleCandidates(candidates []string) []string {
	expanded := make([]string, 0, len(candidates)*2)
	seen := make(map[string]struct{}, len(candidates)*2)
	appendCandidate := func(candidate string) {
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		expanded = append(expanded, candidate)
	}

	for _, candidate := range candidates {
		appendCandidate(candidate)
		parsed, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if hostname == "" || net.ParseIP(hostname) != nil || strings.HasPrefix(hostname, "www.") {
			continue
		}
		registrableDomain, err := publicsuffix.EffectiveTLDPlusOne(hostname)
		if err != nil || !strings.EqualFold(registrableDomain, hostname) {
			continue
		}
		wwwURL := *parsed
		wwwURL.Host = "www." + hostname
		if port := parsed.Port(); port != "" {
			wwwURL.Host = net.JoinHostPort("www."+hostname, port)
		}
		appendCandidate(wwwURL.String())
	}
	return expanded
}

func resolveSameSiteFrameURL(baseValue, frameValue string) (string, error) {
	baseURL, err := url.Parse(baseValue)
	if err != nil || baseURL.Hostname() == "" {
		return "", errors.New("invalid page URL")
	}
	frameReference, err := url.Parse(strings.TrimSpace(frameValue))
	if err != nil {
		return "", errors.New("invalid frame URL")
	}
	frameURL := baseURL.ResolveReference(frameReference)
	if frameURL.Scheme != "http" && frameURL.Scheme != "https" {
		return "", errors.New("frame URL must use http or https")
	}
	if frameURL.User != nil {
		return "", errors.New("frame URL must not contain user information")
	}
	if !sameSiteHostname(baseURL.Hostname(), frameURL.Hostname()) {
		return "", errors.New("cross-site frame URL is not allowed")
	}
	frameURL.Fragment = ""
	return frameURL.String(), nil
}

func sameSiteHostname(left, right string) bool {
	left = strings.ToLower(strings.TrimSuffix(left, "."))
	right = strings.ToLower(strings.TrimSuffix(right, "."))
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	if net.ParseIP(left) != nil || net.ParseIP(right) != nil {
		return false
	}
	leftDomain, leftErr := publicsuffix.EffectiveTLDPlusOne(left)
	rightDomain, rightErr := publicsuffix.EffectiveTLDPlusOne(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftDomain, rightDomain)
}
