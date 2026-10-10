package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

const (
	limitSize int64 = 1024 * 1024 * 1024 * 10 // 允许的文件大小，默认10GB
	host            = "0.0.0.0"               // 监听地址
	port            = 45000                   // 监听端口
)

var (
	// 入口链接：用户可直接粘贴的 GitHub 链接。命中这些的 302 会被重写为代理地址。
	entryExps = []*regexp.Regexp{
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:releases|archive)/.*$`),
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:blob|raw)/.*$`),
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:info|git-).*$`),
		regexp.MustCompile(`^(?:https?://)?raw\.github(?:usercontent|)\.com/([^/]+)/([^/]+)/.+?/.+$`),
		regexp.MustCompile(`^(?:https?://)?gist\.github(?:usercontent|)\.com/([^/]+)/.+?/.+$`),
		regexp.MustCompile(`^(?:https?://)?api\.github\.com/.+?/([^/]+)(?:/.*)?$`),
	}
	// 新增：GitHub 资源 CDN 主机（release 资产签名直链 / 归档分流 / LFS 媒体）。
	// 这些链接可以直接代理，但**不参与 Location 重写**——保证 github.com 的 302
	// 仍在服务端内部跟随，客户端拿到的始终是文件内容而不是真实文件地址。
	assetExps = []*regexp.Regexp{
		// release-assets：新版 release 资产签名直链（302 目标）
		// objects / github-releases：旧版资产 CDN 主机（兼容历史链接）
		regexp.MustCompile(`^(?:https?://)?(?:release-assets|objects|github-releases)\.githubusercontent\.com/([^/]+).*$`),
		// codeload：源码归档（archive / tar.gz / zip）分流主机
		regexp.MustCompile(`^(?:https?://)?codeload\.github\.com/([^/]+)/([^/]+)/.*$`),
		// media：Git LFS 媒体文件
		regexp.MustCompile(`^(?:https?://)?media\.githubusercontent\.com/(?:media/)?([^/]+)/([^/]+)/.*$`),
	}
	// checkURL 使用：入口 + 资源 CDN
	exps       = append(append([]*regexp.Regexp{}, entryExps...), assetExps...)
	httpClient *http.Client
	// config 以不可变快照方式发布：loadConfig 构造新对象后原子替换，
	// 请求路径只做 atomic.Load，避免配置重载与请求处理之间的数据竞争和锁开销。
	config atomic.Pointer[Config]
	log    = logrus.New()

	tmpl    *template.Template
	tmpl404 *template.Template

	// 每个 IP 独立的滑动窗口限流器
	limiter = newRateLimiter()
)

type Config struct {
	WhiteList       []string           `json:"whiteList"`
	BlackList       []string           `json:"blackList"`
	Debug           bool               `json:"debug"`
	AnalyticsURL    string             `json:"analyticsURL"`
	SupportImageURL string             `json:"supportImageURL"`
	RequestLimit    RequestLimitConfig `toml:"requestLimit"`
}

type RequestLimitConfig struct {
	LimitRate int64             `toml:"limitRate"`
	LimitSize int64             `toml:"limitSize"`
	LimitParm map[string]string `toml:"limitParm"`
	LimitAddr []string          `toml:"limitAddr"`
}

func init() {
	formatter := logrus.TextFormatter{
		ForceColors:               true,
		EnvironmentOverrideColors: true,
		TimestampFormat:           "2006-01-02 15:04:05",
		FullTimestamp:             true,
	}
	log.SetFormatter(&formatter)
	// 保证在任何请求到来前 config 非 nil。
	config.Store(&Config{})
}

// rateLimiter 按 IP 记录滑动窗口内的请求时间。
// 相比原来「每个请求都遍历全部 IP」的实现，这里只处理当前 IP，
// 并以固定间隔（sweepEvery）回收长期不活跃的 IP，避免数据结构无界增长。
type rateLimiter struct {
	mu         sync.Mutex
	entries    map[string][]time.Time
	lastSweep  time.Time
	window     time.Duration
	sweepEvery time.Duration
	// now 可注入，默认 time.Now；测试用 allowAt 注入确定性时间。
	now func() time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		entries:    make(map[string][]time.Time),
		lastSweep:  time.Now(),
		window:     time.Minute,
		sweepEvery: time.Minute,
		now:        time.Now,
	}
}

// allow 是生产入口：先取得锁、再采样当前时间，保证时间戳的采样顺序与写入顺序
// 一致，从而维持 entries[ip] 按时间升序的不变量（否则乱序会导致过期记录裁剪
// 与 sweep 回收判断出错）。
func (l *rateLimiter) allow(ip string, limit int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowLocked(ip, limit, l.now())
}

// allowAt 是测试辅助入口，允许注入时间；与 allow 复用同一套加锁及限流逻辑。
func (l *rateLimiter) allowAt(ip string, limit int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowLocked(ip, limit, now)
}

// allowLocked 执行限流判断与记录，必须在持有 l.mu 时调用。
// limit <= 0 表示关闭限流：直接放行且不记录、不回收。
// window 内允许恰好 limit 次请求，第 limit+1 次起返回 false。
func (l *rateLimiter) allowLocked(ip string, limit int64, now time.Time) bool {
	if limit <= 0 {
		return true
	}

	// 低频回收：最多每 sweepEvery 触发一次，避免每请求全表扫描。
	if now.Sub(l.lastSweep) >= l.sweepEvery {
		l.sweepLocked(now)
	}

	cutoff := now.Add(-l.window)
	times := pruneBefore(l.entries[ip], cutoff)
	if len(times) >= int(limit) {
		l.entries[ip] = times
		return false
	}
	l.entries[ip] = append(times, now)
	return true
}

// sweepLocked 删除窗口内已无请求的 IP 键，并裁剪仍活跃 IP 的过期记录。
// 必须在持有 l.mu 时调用。
func (l *rateLimiter) sweepLocked(now time.Time) {
	l.lastSweep = now
	cutoff := now.Add(-l.window)
	for ip, times := range l.entries {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(l.entries, ip)
			continue
		}
		if pruned := pruneBefore(times, cutoff); len(pruned) != len(times) {
			l.entries[ip] = pruned
		}
	}
}

// pruneBefore 丢弃时间戳早于 cutoff 的前缀记录。记录按时间升序追加，
// 因此可安全地从头裁剪；保留 t == cutoff 的记录以对齐原有时间窗口语义。
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

func customLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查请求路径，如果路径是 "/"，则不记录日志
		if c.Request.URL.Path == "/" {
			c.Next() // 继续处理请求，但不记录日志
			return
		}

		// 记录日志（这里使用 Gin 默认的日志格式）
		start := time.Now()
		c.Next()
		end := time.Now()
		latency := end.Sub(start)
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()
		path := c.Request.URL.Path
		userAgent := c.Request.UserAgent()

		logMessage := fmt.Sprintf(
			"%15s | %3d | %8v | %s   \"%s\" | %-50s",
			clientIP, statusCode, latency.Round(time.Millisecond), method, path, userAgent,
		)

		log.Debug(logMessage)
	}
}

func main() {
	loadConfig()
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			loadConfig()
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// 使用自定义日志中间件
	router.Use(customLogger())

	httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          1000,
			MaxIdleConnsPerHost:   1000,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 300 * time.Second,
		},
	}

	var err error
	tmpl, err = template.ParseFiles("./public/index.html")
	if err != nil {
		log.Errorf("load template error: %v\n", err)
		return
	}

	tmpl404, err = template.ParseFiles("./public/404.html")
	if err != nil {
		log.Errorf("load 404 template error: %v\n", err)
		return
	}

	httpBase := fmt.Sprintf("%s:%d", host, port)
	log.Info("start HTTP server @ ", httpBase)

	router.GET("/", func(c *gin.Context) {
		analyticsURL := config.Load().AnalyticsURL

		data := struct {
			AnalyticsURL string
		}{
			AnalyticsURL: analyticsURL,
		}

		if err := tmpl.Execute(c.Writer, data); err != nil {
			c.String(http.StatusInternalServerError, fmt.Sprintf("Error rendering template: %v", err))
		}
	})

	router.NoRoute(handler)

	err = router.Run(fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		log.Errorf("start server error: %v\n", err)
	}
}

func handler(c *gin.Context) {
	// 取一次不可变配置快照，保证整个请求内看到一致的配置，且无锁、无数据竞争。
	cfg := config.Load()

	rawPath := strings.TrimPrefix(c.Request.URL.RequestURI(), "/")

	for strings.HasPrefix(rawPath, "/") {
		rawPath = strings.TrimPrefix(rawPath, "/")
	}

	if !strings.HasPrefix(rawPath, "http") {
		rawPath = "https://" + rawPath
	}

	matches := checkURL(rawPath)
	if matches != nil {
		if len(cfg.WhiteList) > 0 && !checkList(matches, cfg.WhiteList) {
			render404(c, tmpl404, cfg)
			return
		}
		if len(cfg.BlackList) > 0 && checkList(matches, cfg.BlackList) {
			render404(c, tmpl404, cfg)
			return
		}
	} else {
		render404(c, tmpl404, cfg)
		return
	}

	if entryExps[1].MatchString(rawPath) {
		rawPath = strings.Replace(rawPath, "/blob/", "/raw/", 1)
	}

	// 基于时间的速率限制：只处理当前 IP，窗口内允许恰好 limitRate 次。
	// 时间在 limiter.allow 内部持锁后采样，避免并发下时间戳乱序。
	clientIP := c.ClientIP()
	limitRate := cfg.RequestLimit.LimitRate
	if !limiter.allow(clientIP, limitRate) {
		log.Debugf("clientIP: %s  rate limited (limit=%d)", clientIP, limitRate)
		c.String(http.StatusTooManyRequests, "Too Many Requests.")
		return
	}

	// 限制访问 IP
	limitAddr := cfg.RequestLimit.LimitAddr
	if len(limitAddr) > 0 {
		for _, ip := range limitAddr {
			if clientIP == ip {
				c.String(http.StatusBadRequest, "Too Many Requests.")
				return
			}
		}
	}

	// 限制请求参数
	limitParm := cfg.RequestLimit.LimitParm
	if len(limitParm) > 0 {
		for key, value := range limitParm {
			if c.Request.Header.Get(key) == value {
				c.String(http.StatusBadRequest, "Too Many Requests.")
				return
			}
		}
	}

	proxy(c, rawPath, cfg)
}

func proxy(c *gin.Context, u string, cfg *Config) {
	req, err := http.NewRequest(c.Request.Method, u, c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("server error %v", err))
		return
	}

	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Host")

	resp, err := httpClient.Do(req)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("server error %v", err))
		return
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)

	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		size, err := strconv.ParseInt(contentLength, 10, 64)
		if err != nil {
			c.String(http.StatusBadRequest, "Invalid Content-Length")
			return
		}

		limitSize := cfg.RequestLimit.LimitSize * 1024 * 1024 // Convert MB to Bytes

		// 如果 limitSize <= 0 则不限制文件大小
		if limitSize > 0 && size > limitSize {
			c.String(http.StatusRequestEntityTooLarge, "File too large.")
			return
		}
	}

	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Referrer-Policy")
	resp.Header.Del("Strict-Transport-Security")

	// 如果 GitHub 返回 404，显示我们的 404 页面
	if resp.StatusCode == http.StatusNotFound {
		render404(c, tmpl404, cfg)
		return
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	if location := resp.Header.Get("Location"); location != "" {
		// 仅入口链接重写为代理地址；资源 CDN 等跳转目标在服务端内部跟随，
		// 直接返回文件内容（客户端看不到真实文件地址）。
		if isEntryURL(location) {
			c.Header("Location", "/"+location)
		} else {
			proxy(c, location, cfg)
			return
		}
	}

	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		return
	}
}

func loadConfig() {
	_ = loadConfigFrom("config.json")
}

// loadConfigFrom 从指定路径加载配置并原子发布。返回错误便于测试验证。
func loadConfigFrom(path string) error {
	log.Info("loading config...")
	file, err := os.Open(path)
	if err != nil {
		log.Errorf("load config error: %v\n", err)
		return err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {

		}
	}(file)

	var newConfig Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&newConfig); err != nil {
		log.Errorf("decod config error: %v\n", err)
		return err
	}

	// 构造完成后整体原子替换，读方无需加锁，也不会看到半更新的配置。
	config.Store(&newConfig)
	if newConfig.Debug {
		log.SetLevel(logrus.DebugLevel)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}
	return nil
}

func checkURL(u string) []string {
	for _, exp := range exps {
		if matches := exp.FindStringSubmatch(u); matches != nil {
			return matches[1:]
		}
	}
	return nil
}

// isEntryURL 判断是否为「入口」链接（github.com / raw / gist / api）。
// 用于决定 302 是否重写为代理地址；资源 CDN 链接不在其中。
func isEntryURL(u string) bool {
	for _, exp := range entryExps {
		if exp.MatchString(u) {
			return true
		}
	}
	return false
}

func checkList(matches, list []string) bool {
	for _, item := range list {
		if strings.HasPrefix(matches[0], item) {
			return true
		}
	}
	return false
}

func render404(c *gin.Context, tmpl *template.Template, cfg *Config) {
	analyticsURL := cfg.AnalyticsURL
	supportImageURL := cfg.SupportImageURL

	data := struct {
		AnalyticsURL    string
		SupportImageURL string
	}{
		AnalyticsURL:    analyticsURL,
		SupportImageURL: supportImageURL,
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusNotFound)
	tmpl.Execute(c.Writer, data)
}
