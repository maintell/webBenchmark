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
	family      ipFamily
	gate        *limiter
	proxySpoof  bool
	proto       protoMode
}

var (
	h               = flag.Bool("h", false, "show this help")
	count           = flag.Int("c", 0, "concurrent routines; omit or 0 to ramp up automatically until throughput stops improving")
	targetInput     = flag.String("s", "", "target url")
	postContent     = flag.String("p", "", "post content")
	randomPost      = flag.Bool("rp", false, "send random post content on every request")
	randomPostSize  = flag.Int("rps", 1024, "size in bytes of the random post content")
	referer         = flag.String("r", "", "referer url")
	detectLocation  = flag.Bool("d", false, "resolve the real target from the Location header")
	xforwardfor     = flag.Bool("f", true, "randomize X-Forwarded-For and X-Real-IP")
	proxySpoof      = flag.Bool("px", false, "spoof the whole set of proxy headers with a random client address on every request; use it to test whether your own rate limits can be bypassed by a forged address")
	xforwardCountry = flag.String("fc", "cn", "country code used to pick X-Forwarded-For addresses")
	subscribe       = flag.String("sub", "", "subscribe url that returns the target url")
	noReuse         = flag.Bool("nr", false, "open a new TCP connection for every request instead of reusing connections")
	requestTimeout  = flag.Int("t", 30, "per-request timeout in seconds, 0 means no timeout")
	forceIPv4       = flag.Bool("4", false, "force IPv4 only; this is the default")
	forceIPv6       = flag.Bool("6", false, "force IPv6 only instead of the default IPv4")
	protoFlag       = flag.String("proto", "http1", "protocol to benchmark with: http1, http2 or http3 (http3 needs the server to support QUIC)")
	noProbe         = flag.Bool("noprobe", false, "skip the startup protocol capability probe")
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

// ipFamily 限制目标解析出来的地址族。
// 双栈环境里一条地址族可能通但极慢（实测某 VPS 到 Cloudflare 的 IPv6 只有
// 15 KB/s，IPv4 有 514 Mbps），而 Go 默认优先 IPv6，量出来会差几个数量级。
type ipFamily int

const (
	familyAny ipFamily = iota
	familyIPv4
	familyIPv6
)

func (f ipFamily) String() string {
	switch f {
	case familyIPv4:
		return "IPv4"
	case familyIPv6:
		return "IPv6"
	default:
		return "any"
	}
}

// newDialer 把连接重定向到 -i 给出的地址，端口沿用目标 URL 自己的端口，
// 或者按 -4 / -6 只使用指定地址族。
func newDialer(ips ipArray, family ipFamily) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			// 拿不到端口时按原样连接，不要瞎猜 80/443。
			return dialer.DialContext(ctx, network, addr)
		}
		if len(ips) > 0 {
			ip := ips[randIntn(len(ips))]
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		}
		if family == familyAny {
			return dialer.DialContext(ctx, network, addr)
		}
		return dialFamily(ctx, dialer, network, host, port, family)
	}
}

// resolveCache 缓存目标地址解析结果。
// dialFamily 以前每建一条连接就查一次解析器；自动模式爬坡时会连续建很多连接，
// 这些重复查询纯属白花时间，在高 RTT 环境下还会叠加到建连延迟上。
type resolveCacheEntry struct {
	addrs   []net.IPAddr
	expires time.Time
}

const resolveTTL = 60 * time.Second

var (
	resolveMu    sync.Mutex
	resolveTable = make(map[string]resolveCacheEntry)
)

func cachedLookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}

	resolveMu.Lock()
	if e, ok := resolveTable[host]; ok && time.Now().Before(e.expires) {
		addrs := e.addrs
		resolveMu.Unlock()
		return addrs, nil
	}
	resolveMu.Unlock()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	resolveMu.Lock()
	resolveTable[host] = resolveCacheEntry{addrs: addrs, expires: time.Now().Add(resolveTTL)}
	resolveMu.Unlock()
	return addrs, nil
}

// dialFamily 只用指定地址族去连，连不上就换下一个，不会退回另一族。
func dialFamily(ctx context.Context, dialer *net.Dialer, network, host, port string, family ipFamily) (net.Conn, error) {
	addrs, err := cachedLookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	tried := 0
	for _, a := range addrs {
		if family == familyIPv4 && a.IP.To4() == nil {
			continue
		}
		if family == familyIPv6 && a.IP.To4() != nil {
			continue
		}
		tried++
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if tried == 0 {
		return nil, fmt.Errorf("no %s address found for %s, %s", family, host, familyHint(family))
	}
	return nil, lastErr
}

// sharedSessionCache 让所有 worker 共享 TLS 会话票据。
// Go 的 tls.Config 默认 ClientSessionCache 是 nil，也就是完全不做会话复用：
// 每新建一条连接都要走完整握手。在高 RTT 链路上一条握手就要两个来回，
// 而自动模式恰恰会在爬坡阶段连续新建很多连接，这笔开销直接吃掉了爬坡期的吞吐。
var sharedSessionCache = tls.NewLRUClientSessionCache(256)

// newTransport 每个 goroutine 独占一个 Transport，连接池互不干扰。
// HTTP/2 需要显式 ForceAttemptHTTP2：一旦设置了 TLSClientConfig 或 DialContext，
// Go 会保守地把 h2 关掉，那样永远只能测到 HTTP/1.1。
func newTransport(opts *benchOptions) roundTripperCloser {
	if opts.proto == protoHTTP3 {
		return newH3Transport(opts)
	}

	tr := &http.Transport{
		// 压测对象常常是裸 IP 或自签证书，证书链校验在这里没有意义。
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ClientSessionCache: sharedSessionCache,
		},
		ForceAttemptHTTP2:   opts.proto == protoHTTP2,
		DisableKeepAlives:   opts.noReuse,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}
	if len(opts.customIP) > 0 || opts.family != familyAny {
		tr.DialContext = newDialer(opts.customIP, opts.family)
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

		// 自动模式下闸门可以动态收放；固定模式 opts.gate 为 nil，等于没有限制。
		if !opts.gate.Acquire() {
			return
		}
		ok := sendOnce(ctx, client, opts, current)
		opts.gate.Release()

		if !ok {
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
		reportError(err)
		return false
	}

	request.Header.Set("Cookie", RandStringBytesMaskImpr(12))
	request.Header.Set("User-Agent", browser.Android())

	requestReferer := opts.referer
	if requestReferer == "" {
		requestReferer = target
	}
	request.Header.Set("Referer", requestReferer)

	if opts.proxySpoof {
		// 伪造整套代理头：按"代理头里的客户端地址"做限流的服务端会看到每次都是新客户端。
		applyProxyHeaders(request)
	} else if opts.xforwardfor {
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
		// Ctrl+C 打断的在途请求不算失败，也不该刷错误。
		if ctx.Err() != nil {
			return false
		}
		stats.failed.Add(1)
		reportError(err)
		return false
	}

	// quic-go 的 HTTP/3 客户端会把 1xx（例如 Cloudflare 的 103 Early Hints）
	// 当作最终响应返回，若直接计数会把吞吐算成 0，这里显式拦掉。
	if resp.StatusCode < http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		stats.failed.Add(1)
		reportError(fmt.Errorf("got informational status %d instead of a final response", resp.StatusCode))
		return false
	}

	// 只有在响应体读完、请求真正结束时才算成功。
	// 旧写法在拿到响应头时就记成功，读响应体时的超时被直接丢掉，
	// 于是"截断的请求"被当成成功，Loaded 明显低于实际链路能力。
	_, copyErr := io.Copy(io.Discard, &countingReader{r: resp.Body})
	resp.Body.Close()
	if copyErr != nil {
		if ctx.Err() != nil {
			return false
		}
		stats.failed.Add(1)
		reportError(copyErr)
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

// spoofAnnounce 只提示一次，避免每请求刷一行。
var spoofAnnounce sync.Once

// proxyHeaderNames 是常见 CDN / 反向代理用来传递真实客户端地址的头。
// 服务端（或它前面的 WAF）只要信任其中任意一个，按 IP 做的限流就能被伪造地址绕过。
var proxyHeaderNames = []string{"X-Forwarded-For",
	"X-Real-IP",
	"X-Client-IP",
	"Client-IP",
	"True-Client-IP",
	"CF-Connecting-IP",
	"Fastly-Client-IP",
	"X-Cluster-Client-IP",
	"X-Originating-IP",
	"X-Remote-IP",
	"X-Remote-Addr",
	"X-Proxy-User-Ip",
	"X-Original-For",
}

// applyProxyHeaders 用同一个随机地址填满整套代理头。
//
// 只对"信任代理头"的部署有效：如果服务端直接面对客户端、按 TCP 源地址限流，
// 这些头不会改变任何东西——这正是本开关要测出来的结论。
func applyProxyHeaders(request *http.Request) {
	ip := randomForwardedIP()
	if ip == "" {
		return
	}
	spoofAnnounce.Do(func() {
		fmt.Fprintf(os.Stderr, "spoof: every proxy header carries a forged client address (first: %s)\n", ip)
	})

	scheme := request.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}

	for _, name := range proxyHeaderNames {
		request.Header.Set(name, ip)
	}

	// RFC 7239 的标准写法，和上面那堆自定义头格式不同。
	request.Header.Set("Forwarded", fmt.Sprintf("for=%s;proto=%s", ip, scheme))
	request.Header.Set("X-Forwarded-Proto", scheme)
	if host := request.URL.Host; host != "" {
		request.Header.Set("X-Forwarded-Host", host)
		request.Header.Set("X-Original-Host", host)
	}
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
  webBenchmark -s https://some.website
      auto ramp: raise concurrency until throughput stops improving, then hold it
  webBenchmark -c 16 -s https://some.website
      fixed 16 concurrent requests
  webBenchmark -c 16 -s https://some.website -i 10.0.0.1 -i 10.0.0.2
      same, but dial 10.0.0.1 and 10.0.0.2 at random instead of resolving dns
  webBenchmark -c 16 -s https://some.website -r https://referer.url
      benchmark https://some.website with a custom referer
  webBenchmark -c 16 -s https://some.website -H 'regid:123' -H 'sign:Random16'
      attach a fixed regid header and a 16 letter random sign header
  webBenchmark -s https://some.website -proto http3
      benchmark over QUIC, for servers that advertise http3 via Alt-Svc
  webBenchmark -s https://some.website -noprobe
      skip the startup protocol probe (it costs one extra request)

`)
}

// autoConcurrency 判断本次是否进入自动模式。
//
// 默认值就是 0，所以"没写 -c"和"写了 -c 0"是同一个意思：让工具自己去探吞吐上限。
// 只有明确给出正数才按固定并发跑。
func autoConcurrency() bool {
	return *count <= 0
}

// selectedFamily 返回本次要使用的地址族。
//
// 默认强制 IPv4：IPv6 的出网质量普遍参差，经常"能连上但极慢"，
// 而 Go 只要发现 AAAA 就优先用 IPv6 且不会因为慢而回退，
// 于是同一个目标能测出相差几个数量级的结果（实测某 VPS 差约 2000 倍）。
// 作为一个测速工具，默认值必须稳定可解释，所以只有显式 -6 才走 IPv6。
func selectedFamily() ipFamily {
	if *forceIPv6 {
		return familyIPv6
	}
	return familyIPv4
}

// familyHint 在目标没有所需地址族时给出下一步怎么做。
func familyHint(f ipFamily) string {
	if f == familyIPv6 {
		return "drop -6 to use the default IPv4"
	}
	return "add -6 if the target is IPv6 only"
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
	if *forceIPv4 && *forceIPv6 {
		return errors.New("-4 and -6 are mutually exclusive")
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

	mode, err := parseProto(*protoFlag)
	if err != nil {
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

	// 一次普通请求就能回答"这台服务器支持到哪一层协议"，而且不污染压测计数。
	if !*noProbe {
		report, advertisesH3 := probeProtocol(target)
		fmt.Fprintf(os.Stderr, "probe: %s\n", report)
		if mode == protoHTTP3 && !advertisesH3 {
			// h3 只会通过 Alt-Svc 宣告；没有宣告时 QUIC 多半被挡或没开，
			// 提前说清楚，免得用户只看到一片超时。
			fmt.Fprintln(os.Stderr, "warning: the target does not advertise http3 via Alt-Svc, QUIC is probably blocked or not enabled")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(*subscribe) > 0 {
		go subscribeLoop(ctx, *subscribe)
	}
	if len(*targetInput) > 0 && *detectLocation {
		go refreshLocationLoop(ctx, *targetInput)
	}
	if *xforwardfor || *proxySpoof {
		// 后台预热，绝不放到请求路径上，否则拉取慢时会卡住整个压测。
		go preloadCountryCIDR(*xforwardCountry)
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
		family:      selectedFamily(),
		proxySpoof:  *proxySpoof,
		proto:       mode,
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		showStat(ctx)
	}()

	if autoConcurrency() {
		// 没给 -c 就自己找上限：从小并发起步，逐级提升到吞吐不再增长。
		start := autoStartConcurrency
		if len(customIP) > start {
			start = len(customIP)
		}

		// 自动模式下允许用单键随时微调并发；终端不支持 raw 模式时静默降级。
		if restore, ok := startKeyReader(ctx, stop, func(factor float64, label string) {
			if ramp := activeRamp.Load(); ramp != nil {
				ramp.Adjust(factor, label)
			}
		}); ok {
			setInteractive(true)
			defer restore()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			runAutoRamp(ctx, opts, &wg, start)
		}()
	} else {
		routines := *count
		if len(customIP) > 0 && routines < len(customIP) {
			routines = len(customIP)
		}
		setFixedConcurrency(routines)
		for i := 0; i < routines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runBenchmark(ctx, opts, getTargetURL)
			}()
		}
	}

	wg.Wait()
	TerminalWriter.Reset()
}
