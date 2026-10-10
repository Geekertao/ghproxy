package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// trackingBody 记录 Close 是否被调用，用于验证响应体被正确关闭。
type trackingBody struct {
	r      io.Reader
	closed bool
}

func (b *trackingBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func withFakeTransport(t *testing.T, resp *http.Response, body *trackingBody) {
	t.Helper()
	old := httpClient
	httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp.Request = r
			return resp, nil
		}),
		// 阻止自动跟随 302，以便直接观察到 proxy 对 Location 的处理。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(func() { httpClient = old })
}

func newTestContext(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, nil)
	return c, w
}

// 9. 代理应正确转发响应内容，并关闭上游响应体。
func TestProxyForwardsAndClosesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const payload = "hello ghproxy streaming body"
	body := &trackingBody{r: strings.NewReader(payload)}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Length": []string{strconv.Itoa(len(payload))}},
		Body:       body,
	}
	withFakeTransport(t, resp, body)

	c, w := newTestContext(http.MethodGet, "/https://raw.githubusercontent.com/a/b/main/x.txt")
	proxy(c, "https://raw.githubusercontent.com/a/b/main/x.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", w.Code)
	}
	if w.Body.String() != payload {
		t.Fatalf("转发内容错误: %q", w.Body.String())
	}
	if !body.closed {
		t.Fatal("上游响应体未被关闭")
	}
}

// 超过文件大小限制时应返回 413 并关闭响应体（不读取整个文件）。
func TestProxyRejectsOversize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldCfg := config.Load()
	t.Cleanup(func() { config.Store(oldCfg) })
	config.Store(&Config{RequestLimit: RequestLimitConfig{LimitSize: 1}}) // 1 MB

	body := &trackingBody{r: strings.NewReader("tiny")}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Length": []string{strconv.Itoa(2 * 1024 * 1024)}},
		Body:       body,
	}
	withFakeTransport(t, resp, body)

	c, w := newTestContext(http.MethodGet, "/https://raw.githubusercontent.com/a/b/main/big.bin")
	proxy(c, "https://raw.githubusercontent.com/a/b/main/big.bin")

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码应为 413，实际 %d", w.Code)
	}
	if !body.closed {
		t.Fatal("上游响应体未被关闭")
	}
}

// 入口链接的 302 应重写为代理地址，而不是内部跟随。
func TestProxyRewritesEntryLocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := &trackingBody{r: strings.NewReader("")}
	resp := &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://github.com/a/b/releases/download/v1/x.zip"}},
		Body:       body,
	}
	withFakeTransport(t, resp, body)

	c, w := newTestContext(http.MethodGet, "/https://github.com/a/b/releases/download/v1/x.zip")
	proxy(c, "https://github.com/a/b/releases/download/v1/x.zip")

	loc := w.Header().Get("Location")
	if loc != "/https://github.com/a/b/releases/download/v1/x.zip" {
		t.Fatalf("Location 重写错误: %q", loc)
	}
}

// 达到限流上限后，handler 直接返回 429，不进入代理流程。
func TestHandlerReturns429WhenLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldLimiter := limiter
	oldCfg := config.Load()
	t.Cleanup(func() {
		limiter = oldLimiter
		config.Store(oldCfg)
	})

	limiter = newRateLimiter()
	config.Store(&Config{RequestLimit: RequestLimitConfig{LimitRate: 1}})

	// 预先占满该 IP 的唯一额度。
	if !limiter.allow("1.2.3.4", 1, time.Now()) {
		t.Fatal("预填充失败")
	}

	c, w := newTestContext(http.MethodGet, "/https://raw.githubusercontent.com/a/b/main/x.txt")
	c.Request.RemoteAddr = "1.2.3.4:12345"
	handler(c)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("应返回 429，实际 %d", w.Code)
	}
}
