package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestExtractTitleDecodesGB2312HTTPMeta(t *testing.T) {
	want := "南京廖华范文网-教案论文资料答案题库下载(航航棒棒)"
	encodedTitle := []byte{
		0xc4, 0xcf, 0xbe, 0xa9, 0xc1, 0xce, 0xbb, 0xaa, 0xb7, 0xb6,
		0xce, 0xc4, 0xcd, 0xf8, 0x2d, 0xbd, 0xcc, 0xb0, 0xb8, 0xc2,
		0xdb, 0xce, 0xc4, 0xd7, 0xca, 0xc1, 0xcf, 0xb4, 0xf0, 0xb0,
		0xb8, 0xcc, 0xe2, 0xbf, 0xe2, 0xcf, 0xc2, 0xd4, 0xd8, 0x28,
		0xba, 0xbd, 0xba, 0xbd, 0xb0, 0xf4, 0xb0, 0xf4, 0x29,
	}
	prefix := `<script>` + strings.Repeat("a", 1400) + `</script><!doctype html><html><head>`
	body := append([]byte(prefix+`<meta http-equiv="Content-Type" content="text/html; charset=gb2312"><title>`), encodedTitle...)
	body = append(body, []byte(`</title></head></html>`)...)

	title, err := extractTitle(body, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("title = %q, want %q", title, want)
	}
}

func TestExtractTitleKeepsUTF8WithoutCharsetDeclaration(t *testing.T) {
	title, err := extractTitle([]byte(`<html><head><title>未声明编码的中文标题</title></head></html>`), "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != "未声明编码的中文标题" {
		t.Fatalf("title = %q", title)
	}
}

func TestExtractTitleKeepsFirstValidTitleWhenAnotherDocumentIsAppended(t *testing.T) {
	body := []byte(`<!doctype html>
<html><head><title>火车时刻表|火车时刻表查询|火车票查询—-火车吧</title></head><body>首页</body></html>
<!doctype html>
<html><head><title>火车网404</title></head><body>404 页面</body></html>`)

	title, err := extractTitle(body, "text/html; charset=utf-8")
	if err != nil {
		t.Fatal(err)
	}
	if title != "火车时刻表|火车时刻表查询|火车票查询—-火车吧" {
		t.Fatalf("title = %q", title)
	}
}

func TestExtractTitleDecodesUndeclaredGB18030AndKeepsFirstValidTitle(t *testing.T) {
	want := "258中文小说阅读网 - 提供最热门的小说阅读网"
	firstTitle, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	appendedTitle, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("附加页面标题"))
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(`<!doctype html><html><head><title>`), firstTitle...)
	body = append(body, []byte(`</title></head><body>首页</body></html><html><head><title>`)...)
	body = append(body, appendedTitle...)
	body = append(body, []byte(`</title></head><body>附加页面</body></html>`)...)

	title, err := extractTitle(body, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("title = %q", title)
	}
}

func TestExtractTitleKeepsDeclaredWindows1252(t *testing.T) {
	want := "Café déjà vu - München"
	encodedTitle, err := charmap.Windows1252.NewEncoder().Bytes([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(`<html><head><title>`), encodedTitle...)
	body = append(body, []byte(`</title></head></html>`)...)

	title, err := extractTitle(body, "text/html; charset=windows-1252")
	if err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("title = %q", title)
	}
}

func TestExpandTitleCandidatesAddsWWWForRegistrableDomain(t *testing.T) {
	candidates := []string{
		"https://example.com/path?q=1",
		"http://example.com/path?q=1",
		"https://api.example.com/path",
		"http://127.0.0.1:8080/",
	}
	want := []string{
		"https://example.com/path?q=1",
		"https://www.example.com/path?q=1",
		"http://example.com/path?q=1",
		"http://www.example.com/path?q=1",
		"https://api.example.com/path",
		"http://127.0.0.1:8080/",
	}
	if got := expandTitleCandidates(candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
}

func TestFetchTitleContinuesAfterGenericServerTitle(t *testing.T) {
	defaultPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>IIS Windows Server</title></head></html>`))
	}))
	defer defaultPage.Close()

	realSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>兽药招商网-兽药网-兽药代理/批发平台-514193兽药网</title></head></html>`))
	}))
	defer realSite.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := fetchTitle(ctx, testConfig(), []string{defaultPage.URL, realSite.URL})
	if result.Error != "" {
		t.Fatalf("fetch title failed: %s", result.Error)
	}
	if result.Title != "兽药招商网-兽药网-兽药代理/批发平台-514193兽药网" {
		t.Fatalf("title = %q", result.Title)
	}
	if !strings.HasPrefix(result.FinalURL, realSite.URL) {
		t.Fatalf("final URL = %q", result.FinalURL)
	}
}

func TestFetchTitleContinuesAfterCandidateTimeout(t *testing.T) {
	timedOut := make(chan struct{}, 1)
	slowSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		timedOut <- struct{}{}
	}))
	defer slowSite.Close()

	realSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>HTTP 回退标题</title></head></html>`))
	}))
	defer realSite.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := fetchTitleWithCandidateTimeout(ctx, testConfig(), []string{slowSite.URL, realSite.URL}, 50*time.Millisecond)
	if result.Error != "" {
		t.Fatalf("fetch title failed: %s", result.Error)
	}
	if result.Title != "HTTP 回退标题" || !strings.HasPrefix(result.FinalURL, realSite.URL) {
		t.Fatalf("unexpected title result: %+v", result)
	}
	select {
	case <-timedOut:
	default:
		t.Fatal("first candidate was not canceled after its independent timeout")
	}
}

func TestFetchTitleFollowsSameSiteFrameWhenPageTitleIsEmpty(t *testing.T) {
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/content" {
			_, _ = w.Write([]byte(`<html><head><title>兽药招商网</title></head></html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><head><title></title></head><frameset><frame src="/content"></frameset></html>`))
	}))
	defer target.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := fetchTitle(ctx, testConfig(), []string{target.URL})
	if result.Error != "" {
		t.Fatalf("fetch title failed: %s", result.Error)
	}
	if result.Title != "兽药招商网" || result.FinalURL != target.URL+"/content" {
		t.Fatalf("unexpected title result: %+v", result)
	}
}

func TestResolveSameSiteFrameURLRejectsCrossSite(t *testing.T) {
	if _, err := resolveSameSiteFrameURL("https://example.com/", "https://example.net/content"); err == nil {
		t.Fatal("expected cross-site frame URL to be rejected")
	}
	got, err := resolveSameSiteFrameURL("http://514193.com/", "https://www.514193.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://www.514193.com/" {
		t.Fatalf("frame URL = %q", got)
	}
}
