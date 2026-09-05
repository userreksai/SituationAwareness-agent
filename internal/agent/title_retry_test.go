package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type titleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f titleRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type titleResponseBody struct {
	*strings.Reader
	closed bool
}

func (b *titleResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestTitleRetriesOnlyForbiddenWithCurlUserAgent(t *testing.T) {
	wantTitle := "258中文小说阅读网 - 提供最热门的小说阅读网"
	body, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(
		"<title>" + wantTitle + "</title><title>附加错误页面标题</title>"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		statuses []int
		wantErr  string
	}{
		{"success without retry", []int{200}, ""},
		{"forbidden then success", []int{403, 200}, ""},
		{"forbidden only retried once", []int{403, 403}, "returned HTTP 403"},
		{"retry failure is not parsed", []int{403, 500}, "returned HTTP 500"},
		{"unauthorized is not retried", []int{401}, "returned HTTP 401"},
		{"not found is not retried", []int{404}, "returned HTTP 404"},
		{"server error is not retried", []int{500}, "returned HTTP 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			calls := 0
			var original *http.Request
			var firstBody *titleResponseBody
			client := &http.Client{Transport: titleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if calls >= len(test.statuses) {
					t.Fatal("unexpected extra retry")
				}
				if r.Context() != ctx {
					t.Fatal("retry must reuse the candidate context and deadline")
				}
				if calls == 0 {
					original = r.Clone(ctx)
					if r.UserAgent() != "SituationAwareness-Agent/1.0" {
						t.Fatalf("normal User-Agent changed: %q", r.UserAgent())
					}
				} else {
					if !firstBody.closed || firstBody.Len() != len(body) {
						t.Fatal("403 body must be closed without waiting to drain it before retrying")
					}
					if r.UserAgent() != "curl/8.5.0" {
						t.Fatalf("retry User-Agent = %q", r.UserAgent())
					}
					if r.URL.String() != original.URL.String() || r.Method != original.Method {
						t.Fatal("retry changed request URL or method")
					}
					gotHeaders := r.Header.Clone()
					gotHeaders.Set("User-Agent", original.UserAgent())
					if !reflect.DeepEqual(gotHeaders, original.Header) {
						t.Fatalf("retry changed headers other than User-Agent: %v", gotHeaders)
					}
				}
				responseBody := &titleResponseBody{Reader: strings.NewReader(string(body))}
				if calls == 0 {
					firstBody = responseBody
				}
				status := test.statuses[calls]
				calls++
				return &http.Response{
					StatusCode: status, Header: http.Header{"Content-Type": {"text/html"}},
					Body: responseBody, Request: r,
				}, nil
			})}
			result, err := fetchTitleURL(ctx, client, "http://example.com/path?q=1", 1<<20, time.Now())
			if test.wantErr == "" {
				if err != nil || result.Title != wantTitle || result.StatusCode != 200 || result.FinalURL != "http://example.com/path?q=1" {
					t.Fatalf("title result=%+v err=%v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) || result.Title != "" {
				t.Fatalf("title result=%+v err=%v, want %q", result, err, test.wantErr)
			}
			if calls != len(test.statuses) {
				t.Fatalf("requests=%d, want %d", calls, len(test.statuses))
			}
			if len(test.statuses) == 2 && err != nil && !strings.Contains(err.Error(), "curl/8.5.0") {
				t.Fatalf("missing compatibility retry diagnostic: %v", err)
			}
		})
	}
}

func TestTitleForbiddenRetryPreservesRequestErrors(t *testing.T) {
	for _, forbiddenFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial request", true: "compatibility retry"}[forbiddenFirst], func(t *testing.T) {
			calls := 0
			wantErr := errors.New("connection refused")
			client := &http.Client{Transport: titleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if forbiddenFirst && calls == 1 {
					return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("Forbidden")), Header: make(http.Header), Request: r}, nil
				}
				return nil, wantErr
			})}
			_, err := fetchTitleURL(context.Background(), client, "http://example.com/", 1<<20, time.Now())
			if !errors.Is(err, wantErr) {
				t.Fatalf("request error lost: %v", err)
			}
			wantCalls := 1
			if forbiddenFirst {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("requests=%d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestTitleDoesNotRetryForbiddenAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: titleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("Forbidden")), Request: r}, nil
	})}
	result, err := fetchTitleURL(ctx, client, "http://example.com/", 1<<20, time.Now())
	if calls != 1 || err == nil || result.Title != "" {
		t.Fatalf("requests=%d result=%+v err=%v", calls, result, err)
	}
}

func TestTitleForbiddenRetryRespectsCandidateAndTaskDeadlines(t *testing.T) {
	for _, test := range []struct {
		name             string
		totalTimeout     time.Duration
		candidateTimeout time.Duration
		wantNext         bool
	}{
		{"candidate timeout continues", 3 * time.Second, 150 * time.Millisecond, true},
		{"task timeout stops", 150 * time.Millisecond, 3 * time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan string, 8)
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path + " " + r.UserAgent()
				if r.URL.Path == "/next" {
					_, _ = io.WriteString(w, "<title>Next candidate</title>")
					return
				}
				if r.UserAgent() == "SituationAwareness-Agent/1.0" {
					w.WriteHeader(http.StatusForbidden)
					w.(http.Flusher).Flush()
				}
				// The first response has a stalled body; the retry has stalled
				// headers. Both must terminate within the same candidate budget.
				<-r.Context().Done()
			}))
			defer site.Close()
			ctx, cancel := context.WithTimeout(context.Background(), test.totalTimeout)
			defer cancel()
			result := fetchTitleWithCandidateTimeout(ctx, testConfig(), []string{site.URL + "/blocked", site.URL + "/next"}, test.candidateTimeout)
			if test.wantNext {
				if result.Error != "" || result.Title != "Next candidate" || ctx.Err() != nil {
					t.Fatalf("candidate timeout prevented fallback: %+v ctx=%v", result, ctx.Err())
				}
			} else if result.Error == "" || result.Title != "" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("task deadline was ignored: %+v ctx=%v", result, ctx.Err())
			}
			got := make([]string, 0, 3)
			for len(requests) > 0 {
				got = append(got, <-requests)
			}
			want := []string{"/blocked SituationAwareness-Agent/1.0", "/blocked curl/8.5.0"}
			if test.wantNext {
				want = append(want, "/next SituationAwareness-Agent/1.0")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("requests=%v, want %v", got, want)
			}
		})
	}
}

func TestTitleForbiddenRetryFollowsRedirectsAndSameSiteFrames(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/home", http.StatusFound)
			return
		}
		if r.UserAgent() != "curl/8.5.0" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/home" {
			_, _ = io.WriteString(w, `<title></title><iframe src="/content"></iframe>`)
			return
		}
		_, _ = io.WriteString(w, `<title>框架中的原标题</title><title>附加错误页</title>`)
	}))
	defer site.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := fetchTitle(ctx, testConfig(), []string{site.URL})
	if result.Error != "" || result.Title != "框架中的原标题" || result.FinalURL != site.URL+"/content" {
		t.Fatalf("redirect/frame behavior changed after retry: %+v", result)
	}
}
