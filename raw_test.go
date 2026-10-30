package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 预算算错会让请求补得过早或过晚，两侧都有代价：早了多排一个 76MB 响应，
// 晚了白白空转一个空闲窗口。所以这组数字要钉死。
func TestRawEncryptedSize(t *testing.T) {
	cases := []struct {
		in   int64
		want int64
	}{
		{0, 0},
		{-1, 0},
		{1, 1 + 22},                    // 一个记录
		{16384, 16384 + 22},            // 恰好一条
		{16385, 16385 + 44},            // 跨到第二条
		{76366286, 76366286 + 22*4662}, // 目标站那个 76MB 文件
	}
	for _, c := range cases {
		if got := rawEncryptedSize(c.in); got != c.want {
			t.Errorf("rawEncryptedSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// 请求行必须带上 Referer，否则目标站直接 403；Host 与路径也要原样。
func TestBuildRawRequest(t *testing.T) {
	req, err := buildRawRequest("https://sys.jijile.net/pc64/sy/sy64.exe?x=1", "https://sys.jijile.net/", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if req.addr != "sys.jijile.net:443" {
		t.Errorf("addr = %q, want sys.jijile.net:443", req.addr)
	}
	if !req.useTLS {
		t.Error("https target must use TLS")
	}
	line := string(req.line)
	for _, want := range []string{
		"GET /pc64/sy/sy64.exe?x=1 HTTP/1.1",
		"Host: sys.jijile.net",
		"Referer: https://sys.jijile.net/",
		"Connection: keep-alive",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("request line missing %q:\n%s", want, line)
		}
	}

	// 明文 http 不握手，端口取默认 80。
	plain, err := buildRawRequest("http://example.com/a", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.useTLS || plain.addr != "example.com:80" {
		t.Errorf("plain target: tls=%v addr=%q", plain.useTLS, plain.addr)
	}
}

func TestIsDeadline(t *testing.T) {
	if !isDeadline(context.DeadlineExceeded) {
		t.Error("context deadline must be recognised")
	}
	if isDeadline(nil) || isDeadline(context.Canceled) {
		t.Error("nil / cancel must not be treated as an idle timeout")
	}
}

// 端到端：raw 模式必须真的把字节数出来，并且在 ctx 取消后立刻退出。
func TestRawPullCountsBytesAndStops(t *testing.T) {
	const size = 256 << 10
	body := make([]byte, size)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "262144")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	opts := &benchOptions{}
	req, err := buildRawRequest(srv.URL+"/file", "", rawEncryptedSize(size))
	if err != nil {
		t.Fatal(err)
	}
	// httptest 的证书是自签的，rawPull 里本来就 InsecureSkipVerify。
	before := stats.bytes.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rawPull(ctx, opts, req) }()

	select {
	case <-done:
	case <-time.After(6 * time.Second): // 大于 rawIdle，仍未退出就是没响应取消
		t.Fatal("rawPull did not return after the context was cancelled")
	}

	if got := stats.bytes.Load() - before; got < size {
		t.Fatalf("counted %d bytes, want at least %d", got, size)
	}
	if stats.rawSent.Load() < 1 {
		t.Fatal("no request was recorded as sent")
	}
}
