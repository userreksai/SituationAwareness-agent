package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
)

const seoMaxBody = 3 << 20
const seoUserAgent = "seo-monitor/1.0 (daily metrics collector; contact your administrator)"

// SEO accepts a hostname, never an arbitrary URL. The only network destination
// is the fixed Aizhan HTTPS origin; redirects are not followed.
type SEOResult struct {
	SourceBlocked bool       `json:"sourceBlocked,omitempty"`
	URL           string     `json:"url"`
	StatusCode    int        `json:"statusCode"`
	ContentType   string     `json:"contentType"`
	Body          []byte     `json:"body,omitempty"` // JSON base64 preserves original response bytes.
	CheckedAt     time.Time  `json:"checkedAt"`
	RetryAt       *time.Time `json:"retryAt,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type seoFetcher struct {
	client   *http.Client
	slot     chan struct{}
	mu       sync.Mutex
	next     time.Time
	failures int
	blocked  bool
}

func newSEOFetcher() *seoFetcher {
	return &seoFetcher{slot: make(chan struct{}, 1), client: &http.Client{
		Timeout:       time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var seoLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func seoDomain(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", fmt.Errorf("seo target contains invalid characters")
	}
	domain, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), ".")))
	if err != nil || len(domain) > 253 || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("seo target must be a hostname")
	}
	for _, label := range strings.Split(domain, ".") {
		if !seoLabel.MatchString(label) {
			return "", fmt.Errorf("seo target must be a hostname without URL, path or port")
		}
	}
	return domain, nil
}

func (f *seoFetcher) run(parent context.Context, cfg Config, request TaskRequest, timeout time.Duration) (TaskResponse, error) {
	domain, err := seoDomain(request.Target)
	if err != nil {
		return TaskResponse{}, err
	}
	if len(request.Options.Ports) > 0 {
		return TaskResponse{}, fmt.Errorf("seo tasks do not accept ports")
	}
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	result := f.fetch(ctx, "https://www.aizhan.com/cha/"+url.PathEscape(domain)+"/")
	finished := time.Now().UTC()
	hostname, _ := os.Hostname()
	return TaskResponse{Version: "v1", TaskID: request.TaskID, Type: "seo", Target: domain,
		Agent: AgentInfo{Name: cfg.AgentName, Hostname: hostname}, Status: "completed", StartedAt: started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds(),
		Result: ProbeResult{NormalizedTarget: domain, Available: result.Error == "", SEO: &result}}, nil
}

func (f *seoFetcher) fetch(ctx context.Context, target string) SEOResult {
	result := SEOResult{URL: target, CheckedAt: time.Now().UTC()}
	select {
	case f.slot <- struct{}{}:
		defer func() { <-f.slot }()
	default:
		after := time.Now().UTC().Add(10 * time.Second)
		result.RetryAt = &after
		result.Error = "seo collector busy"
		return result
	}
	f.mu.Lock()
	until := f.next
	blocked := f.blocked
	f.mu.Unlock()
	if until.After(time.Now()) {
		result.SourceBlocked = blocked
		result.RetryAt = &until
		result.Error = "seo source is cooling down or rate limited"
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", seoUserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		var resp *http.Response
		resp, err = f.client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			result.StatusCode = resp.StatusCode
			result.ContentType = resp.Header.Get("Content-Type")
			if date := seoRetryAfter(resp.Header.Get("Retry-After")); !date.IsZero() {
				result.RetryAt = &date
			}
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("Aizhan returned HTTP %d", resp.StatusCode)
			} else {
				result.Body, err = io.ReadAll(io.LimitReader(resp.Body, seoMaxBody+1))
				if err == nil && len(result.Body) == 0 {
					err = fmt.Errorf("Aizhan returned HTTP 200 with an empty body")
				}
				if err == nil && len(result.Body) > seoMaxBody {
					err = fmt.Errorf("Aizhan response exceeds 3 MiB")
				}
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		result.Body = nil
		result.Error = err.Error()
		result.SourceBlocked = result.StatusCode == 403 || result.StatusCode == 429 ||
			(result.RetryAt != nil && result.RetryAt.After(time.Now()))
		if !result.SourceBlocked {
			// Transport errors, empty pages and ordinary 5xx responses affect
			// only this task. Keep the normal gap for the next domain.
			f.next = time.Now().UTC().Add(10 * time.Second)
			f.blocked = false
			return result
		}
		f.blocked = true
		f.failures++
		delay := 15 * time.Minute
		for n := 1; n < f.failures && delay < time.Hour; n++ {
			delay *= 2
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		f.next = time.Now().UTC().Add(delay)
		if result.RetryAt != nil && result.RetryAt.After(f.next) {
			f.next = *result.RetryAt
		}
		until := f.next
		result.RetryAt = &until
	} else {
		f.failures = 0
		f.blocked = false
		f.next = time.Now().UTC().Add(10 * time.Second)
	}
	return result
}

func seoRetryAfter(raw string) time.Time {
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n >= 0 {
		if n > 604800 {
			n = 604800
		}
		return time.Now().UTC().Add(time.Duration(n) * time.Second)
	}
	if t, err := http.ParseTime(raw); err == nil && t.After(time.Now()) {
		return t.UTC()
	}
	return time.Time{}
}
