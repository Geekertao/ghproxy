package main

import (
	"bytes"
	"compress/gzip"
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
	// 对发往 GitHub API 的请求放弃客户端的内容编码协商。
	//
	// 原因：客户端（浏览器、带 --compressed 的 curl 等）常带 Accept-Encoding，
	// 该头会被原样转发给上游，于是限流错误体可能是 gzip 压缩的；而压缩字节
	// 无法做 JSON 精准替换，出口 IP 就会继续漏出去。去掉该头后由 Transport
	// 自行加 Accept-Encoding: gzip 并**透明解压**，正文始终是明文。
	// 其余上游（raw / codeload / 资产直链 / 大文件）保持原样转发，流式行为不变。
	if isGitHubAPIHost(req.URL.Host) {
		req.Header.Del("Accept-Encoding")
	}

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

	// 上游 403 / 429 的正文可能带着本服务的出口 IP（GitHub API 限流文案里就
	// 直接写着 "API rate limit exceeded for <ip>"）。这里只做**精准替换**：
	// 仅当目标是 api.github.com、正文是合法 JSON、message 符合已知限流文案且
	// 其中的地址通过 IPv4 校验时，才把那个地址换成占位符，其余正文原样保留。
	// 注意：只有没有 Location 时才缓冲正文，以免干扰重定向分支。
	// 主机取自**最终响应**对应的请求：http.Client 默认跟随重定向，最终响应可能来自
	// 另一个主机；此时不能拿最初请求的主机做判断。主机信息缺失时失败关闭（不脱敏）。
	var redactedBody, bufferedBody []byte
	redacted := false
	if resp.Header.Get("Location") == "" {
		if host, ok := finalResponseHost(resp); ok {
			redactedBody, bufferedBody, redacted = redactGitHubRateLimit(resp, host)
		}
	}

	for key, values := range resp.Header {
		if redacted && strings.EqualFold(key, "Content-Length") {
			continue // 正文已改写，上游的长度不能再沿用
		}
		for _, value := range values {
			c.Header(key, value)
		}
	}
	if redacted {
		c.Writer.Header().Del("Content-Length")
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
	if redacted {
		if _, err := c.Writer.Write(redactedBody); err != nil {
			return
		}
		return
	}
	if len(bufferedBody) > 0 {
		// 为了判断能否改写而读入内存的前缀必须写回，否则会丢数据。
		if _, err := c.Writer.Write(bufferedBody); err != nil {
			return
		}
	}
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

// ---------------------------------------------------------------------------
// 出口 IP 脱敏（精准替换，不重写整段错误正文）
//
// 生产实测：GitHub API 匿名配额耗尽时返回
//   403 {"message":"API rate limit exceeded for 198.98.52.152. (But here's the
//         good news: Authenticated requests get a higher rate limit. ...)",
//         "documentation_url":"https://docs.github.com/..."}
// 其中的地址是**本代理服务器的出口 IP**，原样转发会把它暴露给任何访问者。
//
// 处理方式：只对「发往 api.github.com 且状态码为 403 / 429」的响应，把 message
// 字段里紧跟在 rateLimitMessagePrefix 之后、且通过 IPv4 校验的地址替换为
// proxyServerPlaceholder；其余字段（含 documentation_url）、提示文字与状态码
// 全部保留。任何条件不满足都按原样转发，绝不输出半改的 JSON。
// ---------------------------------------------------------------------------

const (
	// rateLimitMessagePrefix 是 GitHub 限流文案中紧接出口 IP 的前缀。
	rateLimitMessagePrefix = "API rate limit exceeded for "
	// proxyServerPlaceholder 是替换出口 IP 后的写法，句子其余部分保持不变。
	proxyServerPlaceholder = "this proxy Server"
	// maxRedactBodyBytes 是允许缓冲以执行脱敏的正文上限；超过则原样转发，
	// 避免为了改几个字节把大响应体读进内存。
	maxRedactBodyBytes = 1 << 20 // 1 MiB
)

// rateLimitIPExp 匹配紧跟在限流文案前缀后的点分十进制 IPv4（不含末尾句点，
// 句点必须原样保留给客户端）。
var rateLimitIPExp = regexp.MustCompile(`^([0-9]{1,3}(?:\.[0-9]{1,3}){3})`)

// finalResponseHost 返回**最终响应**所对应请求的主机。
//
// 为什么不能直接用最初请求的主机（曾用 req.URL.Host）：http.Client 默认跟随
// 重定向，最终响应可能来自另一个主机；若拿初始主机判断，就等于“初始是
// api.github.com 就脱敏”，而正文实际来自别处，判断依据不成立。
//
// 任何环节缺失（resp / resp.Request / resp.Request.URL 为空，或 Host 为空）都
// 返回 ok=false —— 失败关闭：不脱敏、按原有逻辑转发。刻意**不**回退到初始主机。
func finalResponseHost(resp *http.Response) (string, bool) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return "", false
	}
	host := resp.Request.URL.Host
	if host == "" {
		return "", false
	}
	return host, true
}

// isGitHubAPIHost 判断上游主机是否为 GitHub API。
// 仅对 api.github.com 生效：raw / codeload / 自建镜像等主机即使返回相似文案
// 也不做替换（它们不该出现本服务的出口 IP，且改动面越小越安全）。
func isGitHubAPIHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, "api.github.com")
}

// validIPv4 校验点分十进制 IPv4：四段、各段 0-255、拒绝前导零（非标准写法
// 一律不认，宁可回退到原样转发）。
func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		if len(p) > 1 && p[0] == '0' {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// redactRateLimitMessage 只改写 message 字符串里目标位置的 IP。
// 返回 (改写后的文案, 是否改写)。
func redactRateLimitMessage(msg string) (string, bool) {
	if !strings.HasPrefix(msg, rateLimitMessagePrefix) {
		return "", false
	}
	rest := msg[len(rateLimitMessagePrefix):]
	m := rateLimitIPExp.FindStringSubmatchIndex(rest)
	if m == nil {
		return "", false
	}
	ip := rest[m[2]:m[3]]
	// 地址后必须是句点（已知文案格式）；句点本身保留。
	if !validIPv4(ip) || m[3] >= len(rest) || rest[m[3]] != '.' {
		return "", false
	}
	return rateLimitMessagePrefix + proxyServerPlaceholder + rest[m[3]:], true
}

// redactRateLimitBody 在**不破坏 JSON 结构**的前提下替换 message 中的出口 IP：
// 先按 JSON 对象解析并取出 message 的原始字节，再在原正文里只替换这一处，
// 其余键值（含 documentation_url）按字节原样保留。
// 返回 (新正文, 是否成功改写)。
func redactRateLimitBody(raw []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false // 不是有效的 JSON 对象
	}
	rawMsg, ok := fields["message"]
	if !ok {
		return nil, false
	}
	var msg string
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		return nil, false // message 不是字符串
	}
	newMsg, changed := redactRateLimitMessage(msg)
	if !changed {
		return nil, false
	}

	// 用 SetEscapeHTML(false) 编码，避免顺手把 message 里的字符转义成 \u003c 之类。
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(newMsg); err != nil {
		return nil, false
	}
	newRawMsg := bytes.TrimRight(buf.Bytes(), "\n")

	// 定位 message 值的原始字节：找不到、或出现多次（无法确定改的是哪个字段）
	// 就放弃，回退到原样转发。
	idx := bytes.Index(raw, rawMsg)
	if idx < 0 || bytes.Index(raw[idx+len(rawMsg):], rawMsg) >= 0 {
		return nil, false
	}
	out := make([]byte, 0, len(raw)+len(newRawMsg)-len(rawMsg))
	out = append(out, raw[:idx]...)
	out = append(out, newRawMsg...)
	out = append(out, raw[idx+len(rawMsg):]...)
	return out, true
}

// redactGitHubRateLimit 是脱敏入口。返回约定：
//   - redacted != nil  → 已改写，调用方应改用它作为正文；
//   - buffered != nil  → 未改写，但已把正文（前缀）读入内存，调用方必须先写
//     buffered 再继续从 resp.Body 复制，否则会丢数据；
//   - 都为 nil（且 ok=false）→ 完全没碰正文，按原有流程流式转发。
func redactGitHubRateLimit(resp *http.Response, host string) (redacted, buffered []byte, ok bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil, nil, false
	}
	if !isGitHubAPIHost(host) {
		return nil, nil, false
	}
	// 仍带 Content-Encoding 的正文不能直接做字符串替换。
	//  - gzip：在正文大小上限内安全解压 → 走同一套精准替换 → 重新压缩；
	//  - 其它（br / zstd / 多值等）：无法在无新依赖的前提下安全解码，保持原样转发。
	// 注意：对 api.github.com 已在请求侧放弃编码协商（见 proxy），Transport 会透明
	// 解压并移除该头，因此正常情况下走不到这里；这段是兜底，不是必经路径。
	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		if !strings.EqualFold(enc, "gzip") {
			return nil, nil, false
		}
		return redactGzipBody(resp)
	}
	if n := resp.ContentLength; n > maxRedactBodyBytes {
		return nil, nil, false
	}

	// 允许长度未知（Go 对 gzip 做透明解压时会清掉 Content-Length）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRedactBodyBytes+1))
	if err != nil {
		return nil, raw, false // 读取出错：已读部分不能丢
	}
	if len(raw) > maxRedactBodyBytes {
		return nil, raw, false
	}
	if body, changed := redactRateLimitBody(raw); changed {
		return body, nil, true
	}
	return nil, raw, false
}

// redactGzipBody 在正文大小上限内解压 gzip、执行同一套精准替换，再重新压缩。
//
// 边界的处理原则：任何一步不满足（超限、解压失败、文案不匹配）都回退到
// “已读字节原样写回 + 继续流式复制”，绝不丢字节、不重复、不截断。
// 解压侧用 LimitReader 限住**解压后**大小（防 gzip 炸弹），压缩侧限住**压缩后**
// 大小（防大响应体被整个缓冲）。
func redactGzipBody(resp *http.Response) (redacted, buffered []byte, ok bool) {
	if n := resp.ContentLength; n > maxRedactBodyBytes {
		return nil, nil, false
	}
	comp, err := io.ReadAll(io.LimitReader(resp.Body, maxRedactBodyBytes+1))
	if err != nil {
		return nil, comp, false // 读取出错：已读部分不能丢
	}
	if len(comp) > maxRedactBodyBytes {
		return nil, comp, false // 压缩后已超限：不缓冲，原样转发
	}

	zr, err := gzip.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, comp, false // 不是合法 gzip：原样转发
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(io.LimitReader(zr, maxRedactBodyBytes+1))
	if err != nil {
		return nil, comp, false
	}
	if len(plain) > maxRedactBodyBytes {
		return nil, comp, false // 解压后超限（含 zip bomb）：原样转发
	}

	newPlain, changed := redactRateLimitBody(plain)
	if !changed {
		return nil, comp, false
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(newPlain); err != nil {
		return nil, comp, false
	}
	if err := zw.Close(); err != nil {
		return nil, comp, false
	}
	return buf.Bytes(), nil, true
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
