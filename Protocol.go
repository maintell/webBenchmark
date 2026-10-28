package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// HTTP/3 走 QUIC（UDP），Go 标准库完全不支持，只能借助 quic-go。
// HTTP/2 标准库支持，但一旦设置了 TLSClientConfig 或 DialContext 就会被默认关闭，
// 需要显式打开 ForceAttemptHTTP2。
type protoMode int

const (
	protoHTTP1 protoMode = iota
	protoHTTP2
	protoHTTP3
)

func (p protoMode) String() string {
	switch p {
	case protoHTTP2:
		return "http2"
	case protoHTTP3:
		return "http3"
	default:
		return "http1"
	}
}

func parseProto(s string) (protoMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "http1", "http1.1", "h1":
		return protoHTTP1, nil
	case "http2", "h2":
		return protoHTTP2, nil
	case "http3", "h3", "quic":
		return protoHTTP3, nil
	}
	return protoHTTP1, fmt.Errorf("unknown protocol %q, want http1, http2 or http3", s)
}

// roundTripperCloser 让压测循环不必关心用的是 http.Transport 还是 http3.Transport。
type roundTripperCloser interface {
	http.RoundTripper
	CloseIdleConnections()
}

// newH3Transport 建立基于 QUIC 的 HTTP/3 传输。
// -i 与地址族约束通过 Dial 钩子实现，语义和 http1/http2 路径保持一致。
func newH3Transport(opts *benchOptions) *http3.RoundTripper {
	tr := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if len(opts.customIP) == 0 && opts.family == familyAny {
		return tr
	}

	tr.Dial = func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return quic.DialAddrEarly(ctx, addr, tlsCfg, cfg)
		}

		if len(opts.customIP) > 0 {
			ip := opts.customIP[randIntn(len(opts.customIP))]
			return quic.DialAddrEarly(ctx, net.JoinHostPort(ip, port), tlsCfg, cfg)
		}

		addrs, err := cachedLookup(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		tried := 0
		for _, a := range addrs {
			if opts.family == familyIPv4 && a.IP.To4() == nil {
				continue
			}
			if opts.family == familyIPv6 && a.IP.To4() != nil {
				continue
			}
			tried++
			conn, dialErr := quic.DialAddrEarly(ctx, net.JoinHostPort(a.IP.String(), port), tlsCfg, cfg)
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if tried == 0 {
			return nil, fmt.Errorf("no %s address found for %s, %s", opts.family, host, familyHint(opts.family))
		}
		return nil, lastErr
	}
	return tr
}

// probeProtocol 用一次普通请求回答"这台服务器支持到哪一层协议"。
//
// 判定依据：
//   - ALPN 协商结果告诉我们 h2 能不能用；
//   - Alt-Svc 里带 h3 才是服务端主动宣告了 HTTP/3，这是客户端发现 h3 的标准途径。
//
// 注意 h3 不走 TLS ALPN 协商，所以只能靠 Alt-Svc 判断，不能指望握手阶段看出来。
func probeProtocol(rawURL string) (report string, advertisesH3 bool) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ClientSessionCache: sharedSessionCache,
		},
		ForceAttemptHTTP2: true,
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	defer tr.CloseIdleConnections()

	resp, err := client.Get(rawURL)
	if err != nil {
		return fmt.Sprintf("probe failed: %v", err), false
	}
	defer resp.Body.Close()

	alpn := ""
	if resp.TLS != nil {
		alpn = resp.TLS.NegotiatedProtocol
	}
	if alpn == "" {
		alpn = resp.Proto
	}

	altSvc := resp.Header.Get("Alt-Svc")
	shown := altSvc
	if shown == "" {
		shown = "none"
	}
	advertisesH3 = strings.Contains(altSvc, "h3")

	var verdict string
	switch {
	case advertisesH3:
		verdict = "server advertises http3 (-proto http3)"
	case alpn == "h2":
		verdict = "server supports http2 only (-proto http2)"
	default:
		verdict = "server supports http1 only (-proto http1)"
	}

	return fmt.Sprintf("alpn=%s alt-svc=%s -> %s", alpn, shortText(shown, 60), verdict), advertisesH3
}
