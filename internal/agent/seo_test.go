package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type seoTransport func(*http.Request) (*http.Response, error)

func (f seoTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSEODomainValidation(t *testing.T) {
	for _, bad := range []string{"http://example.com", "localhost", "127.0.0.1", "example.com:80", "example.com/path", "example.com?x=1", "user@example.com", "a..com", "-a.com", "example.com\r\n"} {
		if _, err := seoDomain(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got, err := seoDomain("ITGIRLS.CN."); err != nil || got != "itgirls.cn" {
		t.Fatal(got, err)
	}
}

func TestSEOFixedDestinationAndRateLimit(t *testing.T) {
	f := newSEOFetcher()
	var calls atomic.Int32
	f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != "https://www.aizhan.com/cha/itgirls.cn/" || r.Header.Get("User-Agent") != seoUserAgent {
			t.Error("unexpected upstream", r.URL)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("shared token leaked upstream")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader("<html>result</html>"))}, nil
	})
	cfg := Config{AgentName: "boce", SharedToken: "token"}
	r, err := f.run(context.Background(), cfg, TaskRequest{TaskID: "1", Type: "seo", Target: "itgirls.cn"}, time.Second)
	if err != nil || !r.Result.Available || string(r.Result.SEO.Body) != "<html>result</html>" {
		t.Fatal(r, err)
	}
	r, err = f.run(context.Background(), cfg, TaskRequest{TaskID: "2", Type: "seo", Target: "itgirls.cn"}, time.Second)
	if err != nil || r.Result.Available || r.Result.SEO.RetryAt == nil || calls.Load() != 1 {
		t.Fatal("rate limit failed")
	}
}

func TestSEOEmptyAndLimitedResponse(t *testing.T) {
	for _, status := range []int{200, 403, 429, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newSEOFetcher()
			f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"7200"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			r := f.fetch(context.Background(), "https://www.aizhan.com/cha/itgirls.cn/")
			if r.Error == "" || len(r.Body) > 0 || r.RetryAt == nil || time.Until(*r.RetryAt) < 119*time.Minute {
				t.Fatalf("blocked response accepted %+v", r)
			}
		})
	}
}

func TestSEOBusyAndOversized(t *testing.T) {
	f := newSEOFetcher()
	f.slot <- struct{}{}
	r := f.fetch(context.Background(), "https://www.aizhan.com/cha/itgirls.cn/")
	if r.Error == "" || r.RetryAt == nil {
		t.Fatal("busy request accepted")
	}
	<-f.slot
	f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", seoMaxBody+1)))}, nil
	})
	r = f.fetch(context.Background(), "https://www.aizhan.com/cha/itgirls.cn/")
	if r.Error == "" || len(r.Body) > 0 {
		t.Fatal("oversized body accepted")
	}
}

func TestSEOAPIAuthenticationAndResponseContract(t *testing.T) {
	cfg := Config{AgentName: "boce", SharedToken: "token", MaxConcurrent: 2, DefaultTimeout: time.Second, MaxTimeout: time.Minute}
	h := &handler{cfg: cfg, semaphore: make(chan struct{}, 2), seo: newSEOFetcher()}
	// Use NewHandler for authorization checks; no upstream request is made.
	srv := httptest.NewServer(NewHandler(cfg, nil))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/v1/tasks", "application/json", strings.NewReader(`{"taskId":"1","type":"seo","target":"itgirls.cn"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("missing token accepted")
	}
	h.seo.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<html>中文</html>"))}, nil
	})
	r, err := runTask(context.Background(), cfg, TaskRequest{TaskID: "1", Type: "seo", Target: "itgirls.cn"}, h.seo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TaskResponse
	if err = json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "seo" || decoded.TaskID != "1" || !decoded.Result.Available || string(decoded.Result.SEO.Body) != "<html>中文</html>" {
		t.Fatal("contract mismatch")
	}
	cfg.SharedToken = ""
	if _, err = runTask(context.Background(), cfg, TaskRequest{Type: "seo", Target: "itgirls.cn"}, h.seo); err == nil {
		t.Fatal("unauthenticated deployment accepted seo task")
	}
}

func TestSEOTransientErrorsDoNotCoolWholeSource(t *testing.T) {
	for _, kind := range []string{"timeout", "empty", "503"} {
		t.Run(kind, func(t *testing.T) {
			f := newSEOFetcher()
			f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
				if kind == "timeout" {
					return nil, context.DeadlineExceeded
				}
				status := 200
				if kind == "503" {
					status = 503
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			r := f.fetch(context.Background(), "https://www.aizhan.com/cha/first.com/")
			if r.Error == "" || r.SourceBlocked || f.blocked || f.failures != 0 || time.Until(f.next) > 11*time.Second {
				t.Fatalf("ordinary error opened circuit: %+v", r)
			}
			// Advance only the ordinary request gap; the next domain can succeed.
			f.next = time.Now().Add(-time.Second)
			f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("result"))}, nil
			})
			if r = f.fetch(context.Background(), "https://www.aizhan.com/cha/next.com/"); r.Error != "" {
				t.Fatal(r.Error)
			}
		})
	}
}

func TestSEOExplicitBlockSurvivesNextTask(t *testing.T) {
	f := newSEOFetcher()
	f.client.Transport = seoTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	r := f.fetch(context.Background(), "https://www.aizhan.com/cha/first.com/")
	if !r.SourceBlocked || r.RetryAt == nil || time.Until(*r.RetryAt) < 14*time.Minute {
		t.Fatalf("missing source block: %+v", r)
	}
	r = f.fetch(context.Background(), "https://www.aizhan.com/cha/next.com/")
	if !r.SourceBlocked || r.StatusCode != 0 || r.RetryAt == nil {
		t.Fatalf("cooldown gate lost blocking reason: %+v", r)
	}
}
