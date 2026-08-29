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
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	maxTitleRunes       = 4096
	maxFrameDepth       = 2
	maxFrameCandidates  = 8
	maxCharsetScanBytes = 64 * 1024
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

	titleCandidates := expandTitleCandidates(candidates)
	failures := make([]string, 0, len(titleCandidates))
	var genericFallback *TitleResult
	for _, candidate := range titleCandidates {
		result, err := fetchTitleURL(ctx, client, candidate, cfg.TitleMaxResponseBytes, checkedAt)
		if err == nil {
			if !isGenericPageTitle(result.Title) {
				return result
			}
			if genericFallback == nil {
				copy := result
				genericFallback = &copy
			}
			continue
		}
		failures = append(failures, candidate+": "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	if genericFallback != nil {
		return *genericFallback
	}
	message := strings.Join(failures, "; ")
	if message == "" {
		message = "title request failed"
	}
	return TitleResult{CheckedAt: checkedAt, Error: message}
}

func fetchTitleURL(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time) (TitleResult, error) {
	return fetchTitleURLDepth(ctx, client, target, maxResponseBytes, checkedAt, make(map[string]struct{}), 0)
}

func fetchTitleURLDepth(ctx context.Context, client *http.Client, target string, maxResponseBytes int64, checkedAt time.Time, visited map[string]struct{}, depth int) (TitleResult, error) {
	if _, ok := visited[target]; ok {
		return TitleResult{}, errors.New("frame URL loop detected")
	}
	visited[target] = struct{}{}

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
	document, err := parseTitleDocument(body, response.Header.Get("Content-Type"))
	if err != nil {
		return TitleResult{}, err
	}
	if utf8.RuneCountInString(document.title) > maxTitleRunes {
		return TitleResult{}, fmt.Errorf("title exceeds %d characters", maxTitleRunes)
	}
	finalURL := target
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	result := TitleResult{
		Title:       document.title,
		FinalURL:    finalURL,
		StatusCode:  response.StatusCode,
		ContentType: response.Header.Get("Content-Type"),
		CheckedAt:   checkedAt,
	}

	if depth < maxFrameDepth && (result.Title == "" || isGenericPageTitle(result.Title)) {
		frameFailures := make([]string, 0, len(document.frameSources))
		for _, source := range document.frameSources {
			frameURL, resolveErr := resolveSameSiteFrameURL(finalURL, source)
			if resolveErr != nil {
				frameFailures = append(frameFailures, source+": "+resolveErr.Error())
				continue
			}
			frameResult, frameErr := fetchTitleURLDepth(ctx, client, frameURL, maxResponseBytes, checkedAt, visited, depth+1)
			if frameErr != nil {
				frameFailures = append(frameFailures, frameURL+": "+frameErr.Error())
				continue
			}
			if !isGenericPageTitle(frameResult.Title) {
				return frameResult, nil
			}
		}
		if result.Title == "" && len(frameFailures) > 0 {
			return TitleResult{}, fmt.Errorf("page title is empty; frame lookup failed: %s", strings.Join(frameFailures, "; "))
		}
	}
	if result.Title == "" {
		return TitleResult{}, errors.New("page has no valid title element")
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
				document.title = normalizePageTitle(value.String())
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
				parsedTitle := normalizePageTitle(value.String())
				if document.title == "" && parsedTitle != "" {
					document.title = parsedTitle
				}
				inTitle = false
				value.Reset()
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

func normalizePageTitle(value string) string {
	normalized := normalizeTitle(value)
	if normalized == "" {
		return ""
	}

	suspiciousLatin := 0
	for _, character := range normalized {
		if character > unicode.MaxASCII && character <= '\u00ff' {
			suspiciousLatin++
		}
	}
	if suspiciousLatin < 4 {
		return normalized
	}

	originalBytes, err := charmap.Windows1252.NewEncoder().Bytes([]byte(normalized))
	if err != nil {
		return normalized
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(originalBytes)
	if err != nil || !utf8.Valid(decoded) {
		return normalized
	}
	candidate := normalizeTitle(string(decoded))
	hanCharacters := 0
	for _, character := range candidate {
		if character == utf8.RuneError {
			return normalized
		}
		if unicode.Is(unicode.Han, character) {
			hanCharacters++
		}
	}
	if hanCharacters < 4 {
		return normalized
	}
	return candidate
}

func isGenericPageTitle(title string) bool {
	normalized := strings.ToLower(normalizeTitle(title))
	_, ok := genericPageTitles[normalized]
	return ok
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
