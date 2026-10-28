package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/EDDYCJY/fake-useragent"
	"github.com/apoorvam/goterminal"
)

const version = "0.10"

type benchOptions struct {
	postContent string
	randomPost  bool
	randomSize  int
	referer     string
	xforwardfor bool
	customIP    ipArray
	headers     headersList
	noReuse     bool
	timeout     time.Duration
}

var (
	h               = flag.Bool("h", false, "show this help")
	count           = flag.Int("c", 16, "concurrent routines")
	targetInput     = flag.String("s", "", "target url")
	postContent     = flag.String("p", "", "post content")
	randomPost      = flag.Bool("rp", false, "send random post content on every request")
	randomPostSize  = flag.Int("rps", 1024, "size in bytes of the random post content")
	referer         = flag.String("r", "", "referer url")
	detectLocation  = flag.Bool("d", false, "resolve the real target from the Location header")
	xforwardfor     = flag.Bool("f", true, "randomize X-Forwarded-For and X-Real-IP")
	xforwardCountry = flag.String("fc", "cn", "country code used to pick X-Forwarded-For addresses")
	subscribe       = flag.String("sub", "", "subscribe url that returns the target url")
	noReuse         = flag.Bool("nr", false, "open a new TCP connection for every request instead of reusing connections")
	requestTimeout  = flag.Int("t", 30, "per-request timeout in seconds, 0 means no timeout")
	dnsServer       = flag.String("dns", "8.8.8.8", "dns server used for the target ip display")
	TerminalWriter  = goterminal.New(os.Stdout)
	customIP        ipArray
	headers         headersList
	targetURL       atomic.Value // string
)

func setTargetURL(s string) {
	if s != "" {
		targetURL.Store(s)
	}
}

func getTargetURL() string {
	if v, ok := targetURL.Load().(string); ok {
		return v
	}
	return ""
}

// newDialer 把连接重定向到 -i 给出的地址，端口沿用目标 URL 自己的端口。
// 旧实现拿 "https"/"http" 去匹配 host:port 形式的 addr，两个分支都永远不成立，
// 结果一律拼成 ":80"，HTTPS 目标和非 80 端口目标全部失效。
func newDialer(ips ipArray) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			// 拿不到端口时按原样连接，不要瞎猜 80/443。
			return dialer.DialContext(ctx, network, addr)
		}
		ip := ips[randIntn(len(ips))]
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
	}
}

// newTransport 每个 goroutine 独占一个 Transport，连接池互不干扰。
// 以前 Transport 建在 for 循环里，每个请求都要重新握手，连接池等于不存在。
func newTransport(opts *benchOptions) *http.Transport {
	tr := &http.Transport{
		// 压测对象常常是裸 IP 或自签证书，证书链校验在这里没有意义。
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   opts.noReuse,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}
	if len(opts.customIP) > 0 {
		tr.DialContext = newDialer(opts.customIP)
	}
	return tr
}

// runBenchmark 持续发请求直到 ctx 被取消。
func runBenchmark(ctx context.Context, opts *benchOptions, target func() string) {
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   opts.timeout,
	}

	for {
		if ctx.Err() != nil {
			return
		}

		current := target()
		if current == "" {
			if !waitOrDone(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}

		if !sendOnce(ctx, client, opts, current) {
			// 失败时退让一下，避免变成满核空转的忙循环。
			if !waitOrDone(ctx, 10*time.Millisecond) {
				return
			}
		}
	}
}

// sendOnce 发一次请求，返回是否成功。
func sendOnce(ctx context.Context, client *http.Client, opts *benchOptions, target string) bool {
	method := http.MethodGet
	var body io.Reader
	switch {
	case opts.randomPost:
		method = http.MethodPost
		body = strings.NewReader(RandStringBytesMaskImpr(opts.randomSize))
	case len(opts.postContent) > 0:
		method = http.MethodPost
		body = strings.NewReader(opts.postContent)
	}

	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		stats.failed.Add(1)
		return false
	}

	request.Header.Set("Cookie", RandStringBytesMaskImpr(12))
	request.Header.Set("User-Agent", browser.Android())

	requestReferer := opts.referer
	if requestReferer == "" {
		requestReferer = target
	}
	request.Header.Set("Referer", requestReferer)

	if opts.xforwardfor {
		// 空值不再写进头里，服务端不会再收到一个空的 X-Forwarded-For。
		// randomForwardedIP 永不阻塞，国家列表还没就绪时用完全随机地址。
		if ip := randomForwardedIP(); ip != "" {
			request.Header.Set("X-Forwarded-For", ip)
			request.Header.Set("X-Real-IP", ip)
		}
	}

	applyCustomHeaders(request, opts.headers)

	resp, err := client.Do(request)
	if err != nil {
		stats.failed.Add(1)
		return false
	}

	// 只有在响应体读完、请求真正结束时才算成功。
	// 旧写法在拿到响应头时就记成功，读响应体时的超时被直接丢掉，
	// 于是"截断的请求"被当成成功，Loaded 明显低于实际链路能力。
	_, copyErr := io.Copy(io.Discard, &countingReader{r: resp.Body})
	resp.Body.Close()
	if copyErr != nil {
		stats.failed.Add(1)
		return false
	}
	stats.requests.Add(1)
	return true
}

// countingReader 边读边累加字节数。
// 如果等请求结束再一次性加，长时间下载期间 Loaded 会一直停在旧值，
// 看起来像"没流量"，实际链路早就跑满了。
type countingReader struct {
	r io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		stats.bytes.Add(uint64(n))
	}
	return n, err
}

// applyCustomHeaders 应用 -H。Key 和 Value 都支持 Random<N> 占位符。
func applyCustomHeaders(request *http.Request, list headersList) {
	for _, head := range list {
		key := head.key
		if n, ok := randomCount(key); ok {
			key = RandStringBytesMaskImpr(n)
		}
		value := head.value
		if n, ok := randomCount(value); ok {
			value = RandStringBytesMaskImpr(n)
		}
		request.Header.Set(key, value)
	}
}

func waitOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `webBenchmark version: %s
Usage: webBenchmark [-c concurrent] [-s target] [-p content] [-r refererUrl] [-f] [-i ip] [-H header]

Options:
`, version)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Examples:
  webBenchmark -c 16 -s https://some.website
      benchmark https://some.website using system dns
  webBenchmark -c 16 -s https://some.website -i 10.0.0.1 -i 10.0.0.2
      same, but dial 10.0.0.1 and 10.0.0.2 at random instead of resolving dns
  webBenchmark -c 16 -s https://some.website -r https://referer.url
      benchmark https://some.website with a custom referer
  webBenchmark -c 16 -s https://some.website -H 'regid:123' -H 'sign:Random16'
      attach a fixed regid header and a 16 letter random sign header

`)
}

// validateFlags 把明显写错的参数挡在启动阶段，而不是让它们变成静默空转。
func validateFlags() error {
	if *count < 0 {
		return fmt.Errorf("-c must be >= 0, got %d", *count)
	}
	if *randomPostSize < 0 {
		return fmt.Errorf("-rps must be >= 0, got %d", *randomPostSize)
	}
	if *requestTimeout < 0 {
		return fmt.Errorf("-t must be >= 0, got %d", *requestTimeout)
	}
	for _, ip := range customIP {
		if ip == "" {
			return errors.New("-i requires a non-empty address")
		}
		// IPv6 本身带冒号，只有"既不是合法 IP 又带冒号"才是误把端口写进来了。
		if strings.Contains(ip, ":") && net.ParseIP(ip) == nil {
			return fmt.Errorf("-i %q looks like it contains a port, pass the bare ip and keep the port in -s", ip)
		}
	}
	return nil
}

// resolveTarget 决定压测目标。以前 -s 只有在同时给了 -d 时才会写进 TargetUrl，
// 于是 `webBenchmark -s <url>` 拿到空串，http.NewRequest 全部失败后满核空转。
func resolveTarget() (string, error) {
	if len(*subscribe) > 0 {
		body := Subscribe(*subscribe)
		if body == "" {
			return "", fmt.Errorf("subscribe %s returned no target", *subscribe)
		}
		return pickTarget(body), nil
	}
	if len(*targetInput) == 0 {
		return "", errors.New("no target given, use -s <url> or -sub <url>")
	}
	return pickTarget(*targetInput), nil
}

func pickTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if !*detectLocation {
		return raw
	}
	if loc := GetHttpLocation(raw); loc != "" {
		return loc
	}
	return raw
}

func validateTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid target %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("target %q must start with http:// or https://", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("target %q has no host", raw)
	}
	return nil
}

func main() {
	flag.Var(&customIP, "i", "custom ip address for that domain; multiple addresses are picked at random")
	flag.Var(&headers, "H", "custom header, e.g. -H 'sign:Random16'; a Random<N> key or value expands to N random letters")
	flag.Usage = usage
	flag.Parse()

	if *h {
		flag.Usage()
		return
	}

	if err := validateFlags(); err != nil {
		fmt.Fprintf(os.Stderr, "webBenchmark: %v\n", err)
		os.Exit(2)
	}

	target, err := resolveTarget()
	if err != nil {
		fmt.Fprintf(os.Stderr, "webBenchmark: %v\n", err)
		os.Exit(2)
	}
	if err := validateTarget(target); err != nil {
		fmt.Fprintf(os.Stderr, "webBenchmark: %v\n", err)
		os.Exit(2)
	}
	setTargetURL(target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(*subscribe) > 0 {
		go subscribeLoop(ctx, *subscribe)
	}
	if len(*targetInput) > 0 && *detectLocation {
		go refreshLocationLoop(ctx, *targetInput)
	}
	if *xforwardfor {
		// 后台预热，绝不放到请求路径上，否则拉取慢时会卡住整个压测。
		go preloadCountryCIDR(*xforwardCountry)
	}

	routines := *count
	if len(customIP) > 0 && routines < len(customIP) {
		routines = len(customIP)
	}
	if routines <= 0 {
		routines = 16
	}

	opts := &benchOptions{
		postContent: *postContent,
		randomPost:  *randomPost,
		randomSize:  *randomPostSize,
		referer:     *referer,
		xforwardfor: *xforwardfor,
		customIP:    customIP,
		headers:     headers,
		noReuse:     *noReuse,
		timeout:     time.Duration(*requestTimeout) * time.Second,
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		showStat(ctx)
	}()

	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runBenchmark(ctx, opts, getTargetURL)
		}()
	}

	wg.Wait()
	TerminalWriter.Reset()
}
