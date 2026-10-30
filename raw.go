package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// rawIdle 是 raw 模式等待下一批数据的上限。
// 到点说明这一份响应发完了（或对端卡住），补一个请求继续拉，不记为失败。
const rawIdle = 5 * time.Second

// drainSink 每搬走一块数据就被调一次：调用方在这里记账、按预算补发请求。
// 返回非 nil 表示中止（例如补发请求失败）。
type drainSink func(int64) error

// drainReadFallback 是"读进缓冲再丢掉"的通用实现，没有 splice 的平台走这条路。
func drainReadFallback(conn *net.TCPConn, scratch *[]byte, sink drainSink) error {
	buf := *scratch
	if len(buf) == 0 {
		buf = make([]byte, 64<<10)
		*scratch = buf
	}
	n, err := conn.Read(buf)
	if n > 0 && sink != nil {
		if se := sink(int64(n)); se != nil {
			return se
		}
	}
	return err
}

// rawRequest 是把一次请求预先算好的结果：raw 模式下每个 worker 会反复用它。
type rawRequest struct {
	addr   string // host:port
	host   string // TLS SNI / Host 头
	budget int64  // 一个响应在线上大约产生多少加密字节，0 表示未知
	useTLS bool   // https 才握手；明文 http 直接读就是 raw
	line   []byte // 完整的 HTTP/1.1 请求
}

// rawEncryptedSize 估算 Content-Length 为 n 的响应在线上的加密字节数。
//
// 每个 TLS 记录最多装 16384 字节明文，额外开销是 5 字节记录头 + 1 字节内层类型
// + 16 字节 AEAD tag。估算只是为了知道"什么时候该补请求"，偏大偏小都不致命：
// 偏早 = 多排一个请求（无害），偏晚 = 由空闲超时兜底。
func rawEncryptedSize(n int64) int64 {
	if n <= 0 {
		return 0
	}
	const record = 16384
	const overhead = 22
	records := (n + record - 1) / record
	return n + records*overhead
}

// buildRawRequest 组装请求。Referer 与普通模式保持一致，否则 CDN 直接 403。
func buildRawRequest(rawURL, referer string, budget int64) (*rawRequest, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("raw mode needs a host, got %q", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	uri := u.RequestURI()
	if uri == "" {
		uri = "/"
	}
	if referer == "" {
		referer = rawURL
	}
	line := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nAccept: */*\r\n"+
		"Accept-Encoding: identity\r\nReferer: %s\r\nConnection: keep-alive\r\n\r\n",
		uri, u.Host, randomUserAgent(), referer)
	return &rawRequest{
		addr:   net.JoinHostPort(host, port),
		host:   host,
		budget: budget,
		useTLS: u.Scheme == "https",
		line:   []byte(line),
	}, nil
}

// probeContentLength 用一次 Range 请求问出文件大小，失败则返回 0（退化成空闲驱动）。
func probeContentLength(opts *benchOptions, rawURL, referer string) int64 {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ClientSessionCache: sharedSessionCache},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Range", "bytes=0-0")
	if referer == "" {
		referer = rawURL
	}
	req.Header.Set("Referer", referer)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 206 会在 Content-Range 里给出总长度；200 就直接看 Content-Length。
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		var start, end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err == nil && total > 0 {
			return total
		}
	}
	return resp.ContentLength
}

// rawPull 建一条连接，握手、发请求，然后只数字节不碰内容，直到连接结束。
//
// 不解密是这里的全部意义：省掉 AES-GCM 与明文拷贝，也省掉 HTTP 解析。
// 代价是看不到状态码和响应边界，所以补请求靠"预算 + 空闲"两条规则。
func rawPull(ctx context.Context, opts *benchOptions, req *rawRequest) error {
	dial := newDialer(opts.customIP, opts.family, opts.recvBuf)
	conn, err := dial(ctx, "tcp", req.addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 取消时把连接关掉：raw 模式是直接在 fd 上读，ctx 取消不会打断阻塞读，
	// 不关连接的话 Ctrl+C 要等一个空闲超时才退得出来。
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopped:
		}
	}()

	// 明文 http 目标本来就没有解密，直连即可；https 才需要握手。
	var (
		tcp   *net.TCPConn
		write func([]byte) error
	)
	if req.useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         req.host,
			ClientSessionCache: sharedSessionCache,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return err
		}
		defer tlsConn.Close()
		underlying, ok := tlsConn.NetConn().(*net.TCPConn)
		if !ok {
			return errors.New("raw mode needs a TCP connection")
		}
		tcp = underlying
		write = func(b []byte) error { _, err := tlsConn.Write(b); return err }
	} else {
		plain, ok := conn.(*net.TCPConn)
		if !ok {
			return errors.New("raw mode needs a TCP connection")
		}
		tcp = plain
		write = func(b []byte) error { _, err := conn.Write(b); return err }
	}

	// 请求本身必须走正常写入路径：我们要的是"不解密响应"，不是伪造报文。
	if err := write(req.line); err != nil {
		return err
	}
	stats.rawSent.Add(1)
	rc, err := tcp.SyscallConn()
	if err != nil {
		return err
	}

	st := newDrainState()
	defer st.close()

	// 字节数在搬的同时就记进去；补发请求也在这里做——先凑够一个响应的量，
	// 再发下一个，这样连接永远不会空转等超时。
	var since int64
	sink := func(n int64) error {
		stats.bytes.Add(uint64(n))
		since += n
		if req.budget > 0 && since >= req.budget {
			if werr := write(req.line); werr != nil {
				return werr
			}
			stats.rawSent.Add(1)
			since -= req.budget
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = tcp.SetReadDeadline(time.Now().Add(rawIdle))
		err := drainRaw(rc, tcp, st, sink)
		switch {
		case err == nil:
			continue
		case isDeadline(err):
			// 半天没有数据：这一份多半发完了，补一个请求继续拉。
			if werr := write(req.line); werr != nil {
				return werr
			}
			stats.rawSent.Add(1)
			since = 0
			continue
		default:
			// 取消时 conn 会被主动关掉，这里收到的 EBADF/EOF 不是目标的问题，
			// 不能记进 Failed。
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// runRawBenchmark 是 raw 模式的 worker：与 runBenchmark 同样的闸门与失败语义。
func runRawBenchmark(ctx context.Context, opts *benchOptions, target func() string) {
	var (
		cached    *rawRequest
		cachedURL string
	)
	for {
		if ctx.Err() != nil {
			return
		}
		cur := target()
		if cur == "" {
			if !waitOrDone(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}

		if cached == nil || cur != cachedURL {
			req, err := buildRawRequest(cur, opts.referer, opts.rawBudget)
			if err != nil {
				stats.recordFailure(err, ctx)
				reportError(err)
				if !waitOrDone(ctx, 10*time.Millisecond) {
					return
				}
				continue
			}
			cached, cachedURL = req, cur
		}

		if !opts.gate.Acquire() {
			return
		}
		err := rawPull(ctx, opts, cached)
		opts.gate.Release()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			stats.recordFailure(err, ctx)
			reportError(err)
			if !waitOrDone(ctx, 10*time.Millisecond) {
				return
			}
		}
	}
}
