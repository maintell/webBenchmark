package main

import (
	"context"
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

	dial := newDialer(ipArray{"127.0.0.1"})
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
	client := &http.Client{Transport: transport, Timeout: opts.timeout}

	beforeFailed := stats.failed.Load()
	beforeRequests := stats.requests.Load()

	if sendOnce(context.Background(), client, opts, srv.URL) {
		t.Fatal("a truncated body must not be reported as a successful request")
	}
	if got := stats.failed.Load(); got != beforeFailed+1 {
		t.Fatalf("failed counter = %d, want %d", got, beforeFailed+1)
	}
	if got := stats.requests.Load(); got != beforeRequests {
		t.Fatalf("requests counter = %d, want it to stay at %d", got, beforeRequests)
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
