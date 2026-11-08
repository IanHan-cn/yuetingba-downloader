// ytb_dl.go —— 悦听吧链式音频下载器（Go 版，仅依赖标准库）
//
// 对应 yuetingba-audio-url.user.js 里复现的两个接口：
//
//	GET {base}/api/app/docs-listen/{tingId}/play-info   -> JSON，含 nextTingId / audioUrl / tingTitle
//	GET {base}/api/app/docs-listen/{tingId}/audio-url   -> 纯文本（也兼容 JSON 字符串 / 对象）音频直链
//
// 工作方式：从 -start 指定的那一集开始，读 play-info，下载音频，再沿
// nextTingId（后集）一集一集向后走，直到没有后集、出现环，或达到 -n 指定的集数。
//
// 仅用于你拥有版权或已获授权的内容，请遵守目标站点的服务条款与 robots.txt。
// 默认串行采集、接口请求带间隔、HTTP(HyperText Transfer Protocol)超文本传输协议 429
// 会按服务端给出的 retryAfter 退避；请不要把并发调得过高。
//
// 编译：
//
//	go build -o ytb-dl .
//
// 用法示例：
//
//	# 1) 只看清单不下载（沿后集走完整本）
//	./ytb-dl -start 3a17f2ed-83a0-629e-beb3-69fc75e6745b -dry-run
//
//	# 2) 从当前集开始，向后一共下载 20 集
//	./ytb-dl -start 3a17f2ed-83a0-629e-beb3-69fc75e6745b \
//	    -n 20 -out ./books/绍宋 -cookies ./cookies.txt \
//	    -device-id 'fp1_xxxxxxxxxxxxxxxx'
//
//	# 3) -start 也可以直接给播放页 URL、/book/ting/<uuid> 路径，
//	#    或本地 HTML 文件（自动提取 data-ting-id）
//	./ytb-dl -start ./index.html -n 5 -dry-run
//
// -device-id 取自浏览器 localStorage 的 cn-docsite-user2-guid：
// 在播放页控制台执行 copy(localStorage.getItem('cn-docsite-user2-guid')) 即可获得。
// 服务端按该值做限流，缺省时程序不发这个头（部分接口可能拒绝或更易触发 429）。
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBase = "https://www.yuetingba.cn"
	defaultUA   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	tingIDRe        = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	dataTingIDRe    = regexp.MustCompile(`(?i)data-ting-id=["']([^"']+)["']`)
	unsafeNameRe    = regexp.MustCompile(`[\\/:*?"<>|\r\n\t]+`)
	numberedTitleRe = regexp.MustCompile(`^\d{3,4}[\s_\-.]`)
)

// ------------------------------------------------------------------ 配置

type Config struct {
	start       string
	limit       int // 一共处理多少集，0 表示一直走到没有后集
	out         string
	cookiesFile string
	cookieRaw   string
	deviceID    string
	base        string
	userAgent   string
	proxy       string
	ext         string
	delay       float64
	rateLimit   float64 // KB/s，0 不限
	timeout     int     // 秒：建连 / 响应头超时（不限制下载大文件的总时长）
	concurrency int
	retries     int
	max429      int
	overwrite   bool
	dryRun      bool
	insecure    bool
}

func parseFlags() Config {
	var c Config
	flag.StringVar(&c.start, "start", "", "起始集 tingId / 播放页 URL / 本地 HTML 文件（必填）")
	flag.IntVar(&c.limit, "n", 0, "一共下载多少集；0 = 沿 nextTingId 一直走到没有后集")
	flag.IntVar(&c.limit, "max-items", 0, "同 -n，兼容 Python 版参数名")
	flag.StringVar(&c.out, "out", "./downloads", "输出目录")
	flag.StringVar(&c.cookiesFile, "cookies", "cookies.txt", "Netscape 格式 Cookie 文件（格式见 ./cookies.txt）")
	flag.StringVar(&c.cookieRaw, "cookie", "", "直接传 Cookie 头字符串，如 'a=1; b=2'（仅发给站点域名）")
	flag.StringVar(&c.deviceID, "device-id", "", "X-Ytb-Device 值（localStorage cn-docsite-user2-guid）")
	flag.StringVar(&c.base, "base", defaultBase, "站点 origin")
	flag.StringVar(&c.userAgent, "user-agent", defaultUA, "User-Agent 请求头")
	flag.StringVar(&c.proxy, "proxy", "", "代理地址，如 http://127.0.0.1:7890")
	flag.StringVar(&c.ext, "ext", "", "强制输出扩展名（如 .mp3）；缺省按 URL 路径与 Content-Type 推断")
	flag.Float64Var(&c.delay, "delay", 1.0, "接口请求最小间隔（秒）")
	flag.Float64Var(&c.rateLimit, "rate-limit", 0, "单任务下载限速 KB/s，0 不限")
	flag.IntVar(&c.timeout, "timeout", 30, "建连 / 响应头超时（秒）")
	flag.IntVar(&c.concurrency, "concurrency", 2, "下载并发数（请保持克制）")
	flag.IntVar(&c.retries, "retries", 3, "单集失败重试次数")
	flag.IntVar(&c.max429, "max-429", 4, "429 限流最大重试次数")
	flag.BoolVar(&c.overwrite, "overwrite", false, "已存在的文件也重新下载")
	flag.BoolVar(&c.dryRun, "dry-run", false, "只列清单，不下载")
	flag.BoolVar(&c.insecure, "insecure", false, "关闭 TLS(Transport Layer Security)传输层安全证书校验（仅排查用，不安全）")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `ytb-dl —— 沿 play-info.nextTingId 向后批量下载音频（Go 版）

用法: ytb-dl -start <tingId|URL|HTML文件> [选项]

必需:
  -start        起始集 tingId、/book/ting/<uuid> 链接，或本地播放页 HTML

常用:
  -n N          一共下载 N 集（默认 0 = 一直向后走到没有后集）
  -out DIR      输出目录（默认 ./downloads）
  -cookies FILE Netscape 格式 Cookie 文件（默认 ./cookies.txt，不存在则忽略）
  -cookie STR   直接给 Cookie 头字符串
  -device-id STR X-Ytb-Device，浏览器 localStorage 的 cn-docsite-user2-guid
  -dry-run      只列清单不下载
  -overwrite    已存在也重下

网络与节奏:
  -base URL     站点 origin（默认 %s）
  -delay SEC    接口最小间隔秒（默认 1.0）
  -concurrency N 下载并发（默认 2）
  -retries N    单集重试次数（默认 3）
  -max-429 N    429 重试次数（默认 4）
  -rate-limit KB/s 限速，0 不限
  -timeout SEC  建连/响应头超时（默认 30）
  -ext .mp3     强制扩展名
  -proxy URL    -user-agent STR -insecure

示例:
  ytb-dl -start 3a17f2ed-83a0-629e-beb3-69fc75e6745b -dry-run
  ytb-dl -start ./index.html -n 20 -out ./books/绍宋 -cookies ./cookies.txt -device-id fp1_xxx

仅用于你拥有版权或已获授权的内容。
`, defaultBase)
	}
	flag.Parse()

	if c.start == "" {
		flag.Usage()
		fmt.Fprintln(flag.CommandLine.Output(), "\n错误：缺少 -start")
		os.Exit(2)
	}
	if c.limit < 0 || c.concurrency < 1 || c.retries < 0 || c.delay < 0 || c.rateLimit < 0 || c.timeout < 1 {
		fmt.Fprintln(flag.CommandLine.Output(), "错误：数值参数不合法（需 -n>=0、-concurrency>=1、-retries>=0、-delay>=0、-timeout>=1）")
		os.Exit(2)
	}
	return c
}

// ------------------------------------------------------------------ Cookie（Netscape 格式）

type netCookie struct {
	domain     string
	includeSub bool // 第二列 TRUE，通常配合前导点域名
	path       string
	secure     bool
	expires    int64
	httpOnly   bool
	name       string
	value      string
}

// loadCookies 解析 curl / 浏览器导出的 Netscape HTTP Cookie File。
// 每行 7 列 Tab 分隔：domain  includeSubdomains  path  secure  expiry  name  value
// 兼容 #HttpOnly_ 前缀、空行、# 注释，以及用空格分隔的宽松格式。
func loadCookies(file string) ([]netCookie, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []netCookie
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		ck := netCookie{httpOnly: strings.HasPrefix(line, "#HttpOnly_")}
		if ck.httpOnly {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		}
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}

		var cols []string
		if strings.Contains(line, "\t") {
			cols = strings.SplitN(line, "\t", 7) // value 里可能还有 Tab
		} else {
			parts := strings.Fields(line)
			if len(parts) >= 7 {
				cols = append(parts[:6], strings.Join(parts[6:], " "))
			} else {
				cols = parts
			}
		}
		if len(cols) < 7 {
			return nil, fmt.Errorf("第 %d 行不是合法的 Netscape Cookie 行（需要 7 列）: %q", lineNo, line)
		}
		ck.domain = cols[0]
		ck.includeSub = strings.EqualFold(cols[1], "TRUE")
		ck.path = cols[2]
		if ck.path == "" {
			ck.path = "/"
		}
		ck.secure = strings.EqualFold(cols[3], "TRUE")
		ck.expires, _ = strconv.ParseInt(cols[4], 10, 64)
		ck.name, ck.value = cols[5], cols[6]
		if ck.name == "" {
			continue
		}
		out = append(out, ck)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// match 按域名 / 路径 / secure 判断是否该把这条 Cookie 发给目标 URL。
// 不把站点 Cookie 发给第三方 CDN(Content Delivery Network)内容分发网络，避免泄露。
// 与 Python 版一致，忽略过期时间（导出的文件常常已过期但服务端仍接受）。
func (c netCookie) match(u *url.URL) bool {
	if c.secure && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	domain := strings.ToLower(strings.TrimPrefix(c.domain, "."))
	if domain == "" {
		return false
	}
	if host != domain {
		if !c.includeSub || !strings.HasSuffix(host, "."+domain) {
			return false
		}
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return strings.HasPrefix(p, c.path)
}

// ------------------------------------------------------------------ HTTP 客户端

type Client struct {
	base        *url.URL
	referer     string
	http        *http.Client
	ua          string
	deviceID    string
	cookies     []netCookie
	extraCookie string

	delay   time.Duration
	max429  int
	mu      sync.Mutex
	lastAPI time.Time
}

func newClient(cfg Config) (*Client, error) {
	base, err := url.Parse(cfg.base)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("非法的 -base: %q", cfg.base)
	}

	var cookies []netCookie
	explicitCookies := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "cookies" {
			explicitCookies = true
		}
	})
	if cfg.cookiesFile != "" {
		cks, err := loadCookies(cfg.cookiesFile)
		switch {
		case err == nil:
			cookies = cks
			log.Printf("[Cookie] 已加载 %d 条：%s", len(cookies), cfg.cookiesFile)
		case os.IsNotExist(err) && !explicitCookies:
			// 默认文件不存在时静默跳过
		default:
			return nil, fmt.Errorf("读取 Cookie 文件失败: %w", err)
		}
	}

	timeout := time.Duration(cfg.timeout) * time.Second
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}
	if cfg.proxy != "" {
		pu, err := url.Parse(cfg.proxy)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("非法的 -proxy: %q", cfg.proxy)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	if cfg.insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 仅用于临时排查
		log.Printf("[警告] 已关闭 TLS(Transport Layer Security)传输层安全证书校验，存在中间人攻击风险")
	}

	if cfg.deviceID == "" {
		log.Printf("[提示] 未提供 -device-id，接口可能拒绝或更易触发 429")
	}

	return &Client{
		base:        base,
		referer:     strings.TrimRight(cfg.base, "/") + "/",
		http:        &http.Client{Transport: tr}, // 不设 Client.Timeout：大文件下载不应被整体超时打断
		ua:          cfg.userAgent,
		deviceID:    cfg.deviceID,
		cookies:     cookies,
		extraCookie: strings.TrimSpace(cfg.cookieRaw),
		delay:       time.Duration(cfg.delay * float64(time.Second)),
		max429:      cfg.max429,
	}, nil
}

func (c *Client) apiURL(tingID, kind string) string {
	return strings.TrimRight(c.base.String(), "/") +
		"/api/app/docs-listen/" + url.PathEscape(tingID) + "/" + kind
}

func (c *Client) cookieValueFor(u *url.URL) string {
	var parts []string
	for _, ck := range c.cookies {
		if ck.match(u) {
			parts = append(parts, ck.name+"="+ck.value)
		}
	}
	// -cookie 字符串只发给站点域名，绝不外带
	if c.extraCookie != "" && strings.EqualFold(u.Host, c.base.Host) {
		parts = append(parts, c.extraCookie)
	}
	return strings.Join(parts, "; ")
}

func (c *Client) newRequest(ctx context.Context, rawURL, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", accept)
	req.Header.Set("Referer", c.referer)
	if c.deviceID != "" {
		req.Header.Set("X-Ytb-Device", c.deviceID)
	}
	if v := c.cookieValueFor(req.URL); v != "" {
		req.Header.Set("Cookie", v)
	}
	return req, nil
}

// throttle 让所有接口请求共享一个最小间隔，避免并发触发限流。
func (c *Client) throttle(ctx context.Context) error {
	if c.delay <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.delay - time.Since(c.lastAPI); wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
	c.lastAPI = time.Now()
	return ctx.Err()
}

// doAPI 发一个 GET，遇到 429 按 retryAfter 退避重试；调用方负责关闭 resp.Body。
func (c *Client) doAPI(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := c.throttle(ctx); err != nil {
			return nil, err
		}
		req, err := c.newRequest(ctx, rawURL, accept)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < c.max429 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			sec, hint := retryAfter(body)
			log.Printf("[429] %s 秒后重试%s [%d/%d]", strconv.FormatFloat(sec, 'f', 1, 64), hint, attempt+1, c.max429)
			if err := sleepCtx(ctx, time.Duration((sec+0.5)*float64(time.Second))); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, rawURL, head(body, 160))
		}
		return resp, nil
	}
}

// PlayInfo 是 play-info 的响应。Go 的 JSON(JavaScript Object Notation)JavaScript 对象简谱
// 字段匹配大小写不敏感，因此 NextTingId 能匹配 nextTingId / nextTingID 等写法。
type PlayInfo struct {
	TingID     string `json:"tingId"`
	BookID     string `json:"bookId"`
	BookName   string `json:"bookName"`
	TingTitle  string `json:"tingTitle"`
	AudioURL   string `json:"audioUrl"`
	PrevTingID string `json:"prevTingId"`
	NextTingID string `json:"nextTingId"`
	CoverURL   string `json:"coverUrl"`
}

func parsePlayInfo(body []byte, fallbackID string) (PlayInfo, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return PlayInfo{}, errors.New("play-info 返回空内容")
	}
	var info PlayInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return PlayInfo{}, fmt.Errorf("解析 play-info 失败: %w（响应: %s）", err, head(body, 200))
	}
	// 兼容个别接口把主体包在 data / result 里
	if info.TingID == "" && info.NextTingID == "" && info.AudioURL == "" && info.TingTitle == "" {
		var wrap map[string]json.RawMessage
		if json.Unmarshal(body, &wrap) == nil {
			for _, k := range []string{"data", "result", "Data", "Result", "payload"} {
				var inner PlayInfo
				if raw, ok := wrap[k]; ok && json.Unmarshal(raw, &inner) == nil &&
					(inner.TingID != "" || inner.NextTingID != "" || inner.AudioURL != "") {
					info = inner
					break
				}
			}
		}
	}
	if info.TingID == "" {
		info.TingID = fallbackID
	}
	return info, nil
}

func (c *Client) getPlayInfo(ctx context.Context, tingID string) (PlayInfo, error) {
	resp, err := c.doAPI(ctx, c.apiURL(tingID, "play-info"), "application/json")
	if err != nil {
		return PlayInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return PlayInfo{}, err
	}
	return parsePlayInfo(body, tingID)
}

// normalizeAudioURL 兼容纯文本 / JSON 字符串 / JSON 对象三种 audio-url 响应。
func normalizeAudioURL(text string) (string, error) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, "{") {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			switch t := v.(type) {
			case string:
				s = t
			case map[string]any:
				s = firstStringInMap(t, "audioUrl", "url", "AudioUrl", "URL", "src")
			}
		}
	}
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", fmt.Errorf("返回值不是音频地址: %q", head([]byte(s), 100))
	}
	return s, nil
}

func (c *Client) getAudioURL(ctx context.Context, tingID string) (string, error) {
	resp, err := c.doAPI(ctx, c.apiURL(tingID, "audio-url"), "*/*")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return normalizeAudioURL(string(body))
}

// ------------------------------------------------------------------ 链式采集

type Item struct {
	Index int
	Info  PlayInfo
}

// collect 沿 nextTingId 串行向后走；seen 用来防止接口异常时出现环。
// 中途出错时返回已采集部分 + 错误，由调用方决定继续还是退出。
func collect(ctx context.Context, c *Client, start string, limit int) ([]Item, error) {
	seen := make(map[string]bool)
	var items []Item
	cur := start
	for cur != "" {
		if err := ctx.Err(); err != nil {
			return items, err
		}
		if seen[cur] {
			log.Printf("[提取] 检测到重复 tingId %s，停止遍历", cur)
			break
		}
		seen[cur] = true

		info, err := c.getPlayInfo(ctx, cur)
		if err != nil {
			return items, fmt.Errorf("第 %d 集 (%s) 采集失败: %w", len(items)+1, cur, err)
		}
		items = append(items, Item{Index: len(items) + 1, Info: info})
		next := info.NextTingID
		if next == "" {
			next = "(已是最后一集)"
		}
		log.Printf("[提取] %04d  %s  %s  → 后集 %s", len(items), info.TingID,
			firstNonEmpty(info.TingTitle, "(无标题)"), next)
		if limit > 0 && len(items) >= limit {
			break
		}
		cur = info.NextTingID
	}
	if len(items) == 0 && cur == "" {
		return items, errors.New("起始 tingId 为空，无法采集")
	}
	return items, nil
}

// ------------------------------------------------------------------ 下载

type Result struct {
	Index    int    `json:"index"`
	TingID   string `json:"tingId"`
	BookName string `json:"bookName"`
	Title    string `json:"title"`
	File     string `json:"file,omitempty"`
	Status   string `json:"status"` // ok / skip / failed
	Error    string `json:"error,omitempty"`
}

type Downloader struct {
	cfg          Config
	client       *Client
	outDir       string
	manifestPath string
	mu           sync.Mutex // 保护 existing 与 manifest 写入
	existing     map[string]bool
}

func newDownloader(cfg Config, c *Client) (*Downloader, error) {
	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		return nil, err
	}
	d := &Downloader{
		cfg:          cfg,
		client:       c,
		outDir:       cfg.out,
		manifestPath: filepath.Join(cfg.out, ".manifest.jsonl"),
		existing:     make(map[string]bool),
	}
	entries, err := os.ReadDir(cfg.out)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			d.existing[e.Name()] = true
		}
	}
	return d, nil
}

// hasCompleted 判断某个 base 是否已有成片（任意扩展名，排除 .part）。
func (d *Downloader) hasCompleted(base string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name := range d.existing {
		if strings.HasPrefix(name, base+".") && !strings.HasSuffix(name, ".part") {
			return true
		}
	}
	return false
}

func (d *Downloader) markCompleted(name string) {
	d.mu.Lock()
	d.existing[name] = true
	d.mu.Unlock()
}

func (d *Downloader) writeManifest(r Result) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(d.manifestPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[警告] 写清单失败: %v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
}

// rateWriter 按本次任务已写入的字节数做平均限速。
type rateWriter struct {
	w           io.Writer
	bytesPerSec float64
	start       time.Time
	written     int64
}

func (rw *rateWriter) Write(p []byte) (int, error) {
	n, err := rw.w.Write(p)
	rw.written += int64(n)
	if rw.bytesPerSec > 0 && rw.written > 0 {
		expected := float64(rw.written) / rw.bytesPerSec
		if wait := expected - time.Since(rw.start).Seconds(); wait > 0 {
			if wait > 1 {
				wait = 1
			}
			time.Sleep(time.Duration(wait * float64(time.Second)))
		}
	}
	return n, err
}

// fetchFile 一次性 GET 音频直链，边下边写 .part，完成后按扩展名改名。
// 支持 Range 续传：服务端返回 206 时追加，返回 200 时从头覆盖。
func (d *Downloader) fetchFile(ctx context.Context, rawURL, base string) (string, error) {
	part := filepath.Join(d.outDir, base+".part")
	var pos int64
	if fi, err := os.Stat(part); err == nil {
		pos = fi.Size()
	}

	req, err := d.client.newRequest(ctx, rawURL, "audio/*,*/*")
	if err != nil {
		return "", err
	}
	if pos > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
	}
	resp, err := d.client.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("音频请求 HTTP %d: %s", resp.StatusCode, head(body, 160))
	}

	appendMode := pos > 0 && resp.StatusCode == http.StatusPartialContent
	if !appendMode {
		pos = 0
	}
	ext := d.cfg.ext
	if ext == "" {
		ext = extOf(rawURL, resp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	final := filepath.Join(d.outDir, base+ext)

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	fp, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return "", err
	}
	rw := &rateWriter{w: fp, bytesPerSec: d.cfg.rateLimit * 1024, start: time.Now()}
	written, copyErr := io.Copy(rw, resp.Body)
	closeErr := fp.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}

	total, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if total > 0 && written < total {
		return "", fmt.Errorf("文件不完整 %d/%d 字节（保留 .part 供下次续传）", written, total)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	resumeNote := ""
	if appendMode {
		resumeNote = "（续传）"
	}
	log.Printf("[下载] %s  本次 %d 字节%s", filepath.Base(final), written, resumeNote)
	return final, nil
}

func (d *Downloader) download(ctx context.Context, it Item) Result {
	info := it.Info
	title := safeName(firstNonEmpty(info.TingTitle, info.TingID), "untitled")
	book := safeName(info.BookName, "book")
	prefix := ""
	if !numberedTitleRe.MatchString(title) { // 标题自带 0001_ 时不重复加序号
		prefix = fmt.Sprintf("%04d_", it.Index)
	}
	base := prefix + title

	rec := Result{
		Index: it.Index, TingID: info.TingID, BookName: book, Title: title,
	}
	if !d.cfg.overwrite && d.hasCompleted(base) {
		rec.Status = "skip"
		d.writeManifest(rec)
		return rec
	}

	var lastErr error
	for attempt := 0; attempt <= d.cfg.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			rec.Status, rec.Error = "failed", err.Error()
			d.writeManifest(rec)
			return rec
		}
		// 每次重试都重新取直链，旧签名地址可能已过期；取失败时仍可用上一轮的直链再试一次
		audioURL := strings.TrimSpace(info.AudioURL)
		if audioURL == "" || attempt > 0 {
			u, err := d.client.getAudioURL(ctx, info.TingID)
			if err != nil {
				lastErr = err
			} else {
				audioURL, lastErr = u, nil
			}
		}
		if audioURL != "" {
			final, err := d.fetchFile(ctx, audioURL, base)
			if err == nil {
				rec.Status, rec.File = "ok", filepath.Base(final)
				d.markCompleted(rec.File)
				d.writeManifest(rec)
				return rec
			}
			lastErr = err
		}
		if attempt < d.cfg.retries {
			backoff := time.Duration(minInt(1<<attempt, 10)) * time.Second
			log.Printf("[重试 %d/%d] %s %s: %v", attempt+1, d.cfg.retries, info.TingID, title, lastErr)
			if err := sleepCtx(ctx, backoff); err != nil {
				break
			}
		}
	}
	rec.Status = "failed"
	rec.Error = fmt.Sprint(lastErr)
	d.writeManifest(rec)
	return rec
}

// ------------------------------------------------------------------ 工具函数

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstStringInMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func head(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.ReplaceAll(string(b), "\n", " ")
}

func safeName(s, fallback string) string {
	s = strings.Trim(unsafeNameRe.ReplaceAllString(s, "_"), ". ")
	r := []rune(s)
	if len(r) > 80 {
		r = r[:80]
	}
	s = strings.Trim(string(r), ". ")
	if s == "" {
		return fallback
	}
	return s
}

func extOf(rawURL, contentType string) string {
	if u, err := url.Parse(rawURL); err == nil {
		p := strings.ToLower(u.Path)
		for _, e := range []string{".mp3", ".m4a", ".aac", ".ogg", ".wav", ".flac", ".wma", ".opus"} {
			if strings.HasSuffix(p, e) {
				return e
			}
		}
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/aac":
		return ".aac"
	case "audio/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac":
		return ".flac"
	default:
		return ".bin"
	}
}

func retryAfter(body []byte) (float64, string) {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		sec := 30.0
		switch v := m["retryAfter"].(type) {
		case float64:
			if v > 0 {
				sec = v
			}
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				sec = f
			}
		}
		hint := ""
		if m["limit"] != nil && m["window"] != nil {
			hint = fmt.Sprintf("（上限 %v 次 / %v 秒）", m["limit"], m["window"])
		}
		return sec, hint
	}
	return 30, ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// normalizeStart 允许 -start 传 tingId、播放页 URL、/book/ting/<uuid> 路径或本地 HTML。
func normalizeStart(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("-start 不能为空")
	}
	if fi, err := os.Stat(s); err == nil && !fi.IsDir() {
		b, err := os.ReadFile(s)
		if err != nil {
			return "", err
		}
		if m := dataTingIDRe.FindSubmatch(b); len(m) > 1 {
			return strings.TrimSpace(string(m[1])), nil
		}
		if id := tingIDRe.Find(b); len(id) > 0 {
			return string(id), nil
		}
		return "", fmt.Errorf("在文件 %s 里找不到 data-ting-id 或 tingId", s)
	}
	if id := tingIDRe.FindString(s); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("无法从 %q 解析出 tingId（应形如 3a17f2ed-83a0-629e-beb3-69fc75e6745b）", s)
}

// ------------------------------------------------------------------ main

func main() {
	log.SetFlags(0) // 进度信息走 stderr，不带时间戳
	if err := run(); err != nil {
		log.Printf("[错误] %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()
	start, err := normalizeStart(cfg.start)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	log.Printf("[起点] %s", start)
	if cfg.limit > 0 {
		log.Printf("[范围] 最多 %d 集", cfg.limit)
	} else {
		log.Printf("[范围] 沿 nextTingId 一直向后，直到没有后集")
	}

	// 1) 提取：串行，避免触发限流
	items, err := collect(ctx, client, start, cfg.limit)
	if err != nil {
		log.Printf("[警告] 采集中断: %v（继续处理已采集的 %d 集）", err, len(items))
	}
	if len(items) == 0 {
		return errors.New("没有采集到任何章节，请检查 -start / -cookies / -device-id / -base")
	}
	log.Printf("[提取] 共 %d 集", len(items))

	if cfg.dryRun {
		for _, it := range items {
			u := firstNonEmpty(it.Info.AudioURL, "(需现取 audio-url)")
			if len(u) > 110 {
				u = u[:110] + "…"
			}
			fmt.Printf("%04d  %s  %s  %s\n", it.Index, it.Info.TingID,
				firstNonEmpty(it.Info.TingTitle, ""), u)
		}
		return nil
	}

	// 2) 下载：低并发，接口请求仍共用全局节流
	d, err := newDownloader(cfg, client)
	if err != nil {
		return err
	}
	jobs := make(chan Item)
	results := make(chan Result, len(items))
	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				results <- d.download(ctx, it)
			}
		}()
	}
	go func() {
		for _, it := range items {
			jobs <- it
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	ok, skipped, failed := 0, 0, 0
	for r := range results {
		switch r.Status {
		case "ok":
			ok++
			fmt.Printf("[完成] %s\n", r.File)
		case "skip":
			skipped++
			fmt.Printf("[跳过] %04d %s\n", r.Index, r.Title)
		default:
			failed++
			fmt.Printf("[失败] %04d %s %s: %s\n", r.Index, r.TingID, r.Title, r.Error)
		}
	}
	log.Printf("完成 %d 集，跳过 %d 集，失败 %d 集；清单: %s", ok, skipped, failed, d.manifestPath)
	if failed > 0 {
		return fmt.Errorf("%d 集下载失败（可原样重跑，会跳过已完成的集并续传 .part）", failed)
	}
	return nil
}
