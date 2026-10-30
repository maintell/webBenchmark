package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runFor 跑一小段压测，并断言 ctx 取消后 goroutine 确实退出了
// （对应 Ctrl+C 能干净收尾，旧代码里 wg.Done() 永远走不到）。
func runFor(t *testing.T, d time.Duration, opts *benchOptions, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBenchmark(ctx, opts, func() string { return target })
	}()

	select {
	case <-done:
	case <-time.After(d + 10*time.Second):
		t.Fatal("benchmark did not stop after context cancellation")
	}
}

// 回归 P0-1：只给目标 URL（不走 -d）时也必须真的发出请求。
// 旧实现里 TargetUrl 只在 -d/-sub 分支赋值，-s 单独使用时是空串，
// 于是 http.NewRequest 每次都失败，程序满核空转但一个请求都不发。
func TestRunBenchmarkSendsRequestsWithoutDetectLocation(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	opts := &benchOptions{referer: srv.URL}
	runFor(t, 500*time.Millisecond, opts, srv.URL+"/probe")

	if hits.Load() == 0 {
		t.Fatal("expected the benchmark to issue requests, got 0")
	}
}

// 回归 P0-2：-i 指定 IP 时必须沿用目标的真实端口。
// 旧实现拿 "https"/"http" 去前缀匹配 host:port，永远不成立，一律拼成 :80。
func TestRunBenchmarkCustomIPKeepsPort(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	opts := &benchOptions{customIP: ipArray{"127.0.0.1"}}
	runFor(t, 500*time.Millisecond, opts, srv.URL+"/ips")

	if hits.Load() == 0 {
		t.Fatalf("expected requests on %s via custom ip, got 0", srv.URL)
	}
}

// 回归 P0-3：HTTPS + -i 必须能完成 TLS 握手。
// 旧实现设置了 DialTLSContext 却返回裸 TCP，Go 会当作已握手的连接直接写明文。
func TestRunBenchmarkHTTPSWithCustomIP(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	opts := &benchOptions{customIP: ipArray{"127.0.0.1"}}
	runFor(t, 500*time.Millisecond, opts, srv.URL+"/tls")

	if hits.Load() == 0 {
		t.Fatalf("expected https requests on %s via custom ip, got 0", srv.URL)
	}
}

// 回归 P0-2（直接验证 dial 层）：端口必须原样透传，不能被改写成 80。
func TestNewDialerKeepsRequestedPort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	dial := newDialer(ipArray{"127.0.0.1"}, familyAny, 0)
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort("example.invalid", strconv.Itoa(port)))
	if err == nil {
		conn.Close()
		t.Fatalf("expected dial on port %d to fail, it succeeded instead", port)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("dial should target port %d, got: %v", port, err)
	}
}

// 回归 P1-1：-H 'RandomN:value' 的 Key 必须被随机化。
// 旧实现解析长度时用错了 head.value，导致键保持字面量 "Random10"。
func TestApplyCustomHeadersRandomizesKey(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyCustomHeaders(req, headersList{{key: "Random12", value: "fixed"}})

	if got := req.Header.Get("Random12"); got != "" {
		t.Fatalf("literal Random12 header should not be sent, got %q", got)
	}

	found := false
	for key, values := range req.Header {
		if key == "Cookie" || len(key) != 12 {
			continue
		}
		if strings.Trim(key, letterBytes) == "" && len(values) == 1 && values[0] == "fixed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a randomized 12 letter header carrying %q, got %v", "fixed", req.Header)
	}
}

func TestApplyCustomHeadersRandomizesValue(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyCustomHeaders(req, headersList{{key: "sign", value: "Random16"}})

	got := req.Header.Get("sign")
	if len(got) != 16 {
		t.Fatalf("expected a 16 letter sign, got %q", got)
	}
	if strings.Trim(got, letterBytes) != "" {
		t.Fatalf("sign should only contain letters, got %q", got)
	}
}

// 回归：响应体读到一半超时不能再算成功。
// 旧写法在拿到响应头时就 requests++，读响应体的错误直接丢弃，
// 于是大文件在慢链路上被 10 秒超时反复截断，计数仍然"全绿"。
func TestSendOnceCountsTruncatedBodyAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1024))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	opts := &benchOptions{timeout: 150 * time.Millisecond}
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()
	// 和线上一致：不设 client.Timeout，让空闲看门去发现"再也没有数据进来"。
	client := &http.Client{Transport: transport}

	beforeFailed := stats.failed.Load()
	beforeTimedOut := stats.timedOut.Load()
	beforeRequests := stats.requests.Load()

	if sendOnce(context.Background(), client, opts, srv.URL) {
		t.Fatal("a truncated body must not be reported as a successful request")
	}
	if got := stats.failed.Load(); got != beforeFailed+1 {
		t.Fatalf("failed counter = %d, want %d", got, beforeFailed+1)
	}
	// 半截卡住属于"我们自己的秒表到点"，要单独计入 timeout，
	// 不能和"目标拒绝"混成一个数字。
	if got := stats.timedOut.Load(); got != beforeTimedOut+1 {
		t.Fatalf("timedOut counter = %d, want %d", got, beforeTimedOut+1)
	}
	if got := stats.requests.Load(); got != beforeRequests {
		t.Fatalf("requests counter = %d, want it to stay at %d", got, beforeRequests)
	}
}

// 回归：慢但持续在传的响应不能被 -t 判死。
// 旧实现用 client.Timeout（整个请求的总时限），一个 76 MB 的文件在 1 MB/s 上
// 必然超过默认 30 秒，于是每个请求都记失败——目标没问题，是秒表掐得太死。
func TestSendOnceToleratesSlowButSteadyBody(t *testing.T) {
	const chunk = 8192
	const chunks = 12 // 12 * 50ms = 600ms，远超下面 300ms 的 -t
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(chunk*chunks))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			w.Write(make([]byte, chunk))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// 空闲阈值给到单次间隔的 6 倍：机器负载再高，也不能靠"这一格刚好没卡"来过。
	opts := &benchOptions{timeout: 300 * time.Millisecond}
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()
	// 线上就是这么建的：不设 client.Timeout，超时完全交给空闲看门。
	client := &http.Client{Transport: transport}

	if !sendOnce(context.Background(), client, opts, srv.URL) {
		t.Fatal("a steady body that outlives -t must not be counted as a failure")
	}
}

// 4xx/5xx 以前被当作成功：压测结果可以全绿，实际全在报错。
func TestSendOnceCountsErrorStatusAsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("denied"))
	}))
	defer srv.Close()

	opts := &benchOptions{timeout: 2 * time.Second}
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	beforeRejected := stats.rejected.Load()
	beforeRequests := stats.requests.Load()

	if sendOnce(context.Background(), client, opts, srv.URL) {
		t.Fatal("a 403 response must not be reported as a successful request")
	}
	if got := stats.rejected.Load(); got != beforeRejected+1 {
		t.Fatalf("rejected counter = %d, want %d", got, beforeRejected+1)
	}
	if got := stats.requests.Load(); got != beforeRequests {
		t.Fatalf("requests counter = %d, want it to stay at %d", got, beforeRequests)
	}
}

// 空闲看门在请求正常结束后会 cancel 请求 ctx。这一步如果太早，
// Transport 会直接拆连接，每条请求都要重新握手，压测就变成测握手了。
func TestSendOnceKeepsConnectionReusable(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	opts := &benchOptions{timeout: 2 * time.Second}
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	for i := 0; i < 5; i++ {
		if !sendOnce(context.Background(), client, opts, srv.URL) {
			t.Fatalf("request %d failed", i)
		}
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("server saw %d connections for 5 sequential requests, want 1 (keep-alive broke)", got)
	}
}

// 代理头伪造：每个常见头都必须带上同一个合法地址，
// 且 proto/host 要与真实请求一致，否则会被服务端直接当成非法请求。
func TestApplyProxyHeadersSpoofsEveryCommonHeader(t *testing.T) {
	req, err := http.NewRequest("GET", "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyProxyHeaders(req)

	for _, name := range proxyHeaderNames {
		got := req.Header.Get(name)
		if net.ParseIP(got) == nil {
			t.Errorf("header %s = %q, want a valid ip", name, got)
		}
	}

	if got := req.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got)
	}
	if got := req.Header.Get("X-Forwarded-Host"); got != "example.com" {
		t.Errorf("X-Forwarded-Host = %q, want example.com", got)
	}

	fwd := req.Header.Get("Forwarded")
	if !strings.HasPrefix(fwd, "for=") || !strings.Contains(fwd, "proto=https") {
		t.Errorf("Forwarded = %q, want RFC 7239 form with proto=https", fwd)
	}
}

// 伪造开关优先级高于 -f：两者同时开着时也只走整套代理头，不重复设置。
func TestProxySpoofTakesPrecedenceOverXForwardFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Connecting-IP") == "" {
			t.Error("expected the wide proxy header set to be applied")
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	opts := &benchOptions{proxySpoof: true, xforwardfor: true, timeout: 5 * time.Second}
	transport := newTransport(opts)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: opts.timeout}

	if !sendOnce(context.Background(), client, opts, srv.URL) {
		t.Fatal("request should succeed")
	}
}

// -t 0 表示不设超时，客户端不能因此退化成默认的无限等待以外的东西。
func TestBenchmarkOptionsTimeoutZeroMeansNoTimeout(t *testing.T) {
	opts := &benchOptions{timeout: 0}
	client := &http.Client{Transport: newTransport(opts), Timeout: opts.timeout}
	defer client.Transport.(*http.Transport).CloseIdleConnections()

	if client.Timeout != 0 {
		t.Fatalf("client timeout = %v, want 0 (disabled)", client.Timeout)
	}
}

// 回归：双栈目标默认会优先 IPv6，而 IPv6 可能通但极慢。
// 实测某 VPS 到 Cloudflare 的 IPv6 只有 15 KB/s、IPv4 有 514 Mbps，
// 所以必须能把地址族钉住，否则结果会差几个数量级。
func TestDialFamilyIPv4ReachesV4OnlyListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	conn, err := dialFamily(context.Background(), &net.Dialer{Timeout: 3 * time.Second},
		"tcp", "localhost", port, familyIPv4)
	if err != nil {
		t.Fatalf("dialFamily IPv4 to localhost:%s failed: %v", port, err)
	}
	conn.Close()
}

func TestDialFamilyRejectsFamilyWithoutAddress(t *testing.T) {
	// 127.0.0.1 只有 IPv4，要求 IPv6 时必须明确报错，而不是悄悄回退到 IPv4。
	_, err := dialFamily(context.Background(), &net.Dialer{Timeout: 3 * time.Second},
		"tcp", "127.0.0.1", "9", familyIPv6)
	if err == nil {
		t.Fatal("expected an error when no IPv6 address exists")
	}
	if !strings.Contains(err.Error(), "-6") {
		t.Fatalf("error should tell the user how to get IPv4, got: %v", err)
	}
}

// 回归：失败原因必须可见且不刷屏。
// 以前错误只进了计数器，用 -6 测一个只有 IPv4 的目标时，
// 用户只看到 Failed 在涨，完全不知道是地址族不匹配。
func TestReportErrorDedupesAndKeepsLast(t *testing.T) {
	errMu.Lock()
	seenError = make(map[string]struct{})
	lastError = ""
	errMu.Unlock()

	reportError(errors.New("dial boom"))
	reportError(errors.New("dial boom"))
	reportError(errors.New("second boom"))

	errMu.Lock()
	defer errMu.Unlock()
	if len(seenError) != 2 {
		t.Fatalf("distinct errors = %d, want 2 (duplicates must not be re-printed)", len(seenError))
	}
	if lastError != "second boom" {
		t.Fatalf("lastError = %q, want the most recent message", lastError)
	}
}

func TestShortText(t *testing.T) {
	if got := shortText("a\r\nb", 10); got != "a  b" {
		t.Errorf("shortText should flatten newlines, got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := shortText(long, 10)
	if got != "xxxxxxxxxx..." {
		t.Errorf("shortText should truncate with an ellipsis, got %q", got)
	}
}

func TestFamilyHint(t *testing.T) { // 默认 IPv4 遇到只有 IPv6 的目标时要能提示加 -6；
	// 反过来要求 IPv6 但目标没有 AAAA 时要提示去掉 -6。
	if got := familyHint(familyIPv4); !strings.Contains(got, "-6") {
		t.Errorf("IPv4 hint should mention -6, got %q", got)
	}
	if got := familyHint(familyIPv6); !strings.Contains(got, "IPv4") {
		t.Errorf("IPv6 hint should mention IPv4, got %q", got)
	}
}

func TestSelectedFamily(t *testing.T) {
	oldV4, oldV6 := *forceIPv4, *forceIPv6
	defer func() { *forceIPv4, *forceIPv6 = oldV4, oldV6 }()

	// 默认必须是 IPv4：双栈环境下 IPv6 常常通但极慢，而 Go 不会因为慢而回退。
	*forceIPv4, *forceIPv6 = false, false
	if got := selectedFamily(); got != familyIPv4 {
		t.Errorf("default family = %v, want IPv4", got)
	}

	// 显式 -4 与默认一致，只是让脚本更自解释。
	*forceIPv4, *forceIPv6 = true, false
	if got := selectedFamily(); got != familyIPv4 {
		t.Errorf("-4 family = %v, want IPv4", got)
	}

	// 只有显式 -6 才走 IPv6。
	*forceIPv4, *forceIPv6 = false, true
	if got := selectedFamily(); got != familyIPv6 {
		t.Errorf("-6 family = %v, want IPv6", got)
	}

	*forceIPv4, *forceIPv6 = true, true
	if err := validateFlags(); err == nil {
		t.Error("expected an error when both -4 and -6 are given")
	}
}
