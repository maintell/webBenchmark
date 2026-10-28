package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProto(t *testing.T) {
	cases := []struct {
		in   string
		want protoMode
		ok   bool
	}{
		{"http1", protoHTTP1, true},
		{"HTTP1.1", protoHTTP1, true},
		{"h1", protoHTTP1, true},
		{"", protoHTTP1, true},
		{"http2", protoHTTP2, true},
		{"h2", protoHTTP2, true},
		{" http3 ", protoHTTP3, true},
		{"h3", protoHTTP3, true},
		{"quic", protoHTTP3, true},
		{"http4", protoHTTP1, false},
		{"tcp", protoHTTP1, false},
	}
	for _, c := range cases {
		got, err := parseProto(c.in)
		if (err == nil) != c.ok {
			t.Errorf("parseProto(%q) err = %v, want ok=%v", c.in, err, c.ok)
		}
		if got != c.want {
			t.Errorf("parseProto(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestProtoModeString(t *testing.T) {
	if got := protoHTTP1.String(); got != "http1" {
		t.Errorf("protoHTTP1 = %q", got)
	}
	if got := protoHTTP2.String(); got != "http2" {
		t.Errorf("protoHTTP2 = %q", got)
	}
	if got := protoHTTP3.String(); got != "http3" {
		t.Errorf("protoHTTP3 = %q", got)
	}
}

func TestProbeProtocolReportsHTTP1Only(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	got, advertises := probeProtocol(srv.URL)
	if !strings.Contains(got, "http1 only") {
		t.Fatalf("probe = %q, want it to report http1 only", got)
	}
	if advertises {
		t.Error("a plain http server must not be reported as advertising http3")
	}
}

// 服务器接受 h2 时，探测必须报出 alpn=h2，否则用户会以为只能跑 http1。
func TestProbeProtocolDetectsHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	got, advertises := probeProtocol(srv.URL)
	if !strings.Contains(got, "alpn=h2") {
		t.Fatalf("probe = %q, want alpn=h2", got)
	}
	if !strings.Contains(got, "http2 only") {
		t.Fatalf("probe = %q, want it to report http2 only", got)
	}
	if advertises {
		t.Error("no Alt-Svc was sent, so http3 must not be reported as advertised")
	}
}

// h3 不走 TLS ALPN，只能靠 Alt-Svc 判断，这里钉住这个判据。
func TestProbeProtocolDetectsAltSvcH3(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	got, advertises := probeProtocol(srv.URL)
	if !strings.Contains(got, "advertises http3") {
		t.Fatalf("probe = %q, want it to report http3 advertisement", got)
	}
	if !advertises {
		t.Error("probe must report the http3 advertisement to the caller")
	}
}

func TestProbeProtocolReportsFailure(t *testing.T) {
	// 端口 1 连不上，探测必须给出可读的失败原因而不是空串。
	got, advertises := probeProtocol("https://127.0.0.1:1/none")
	if !strings.Contains(got, "probe failed") {
		t.Fatalf("probe = %q, want a failure message", got)
	}
	if advertises {
		t.Error("a failed probe must not claim http3 support")
	}
}

func TestNewTransportSelectsProtocol(t *testing.T) {
	h1, ok := newTransport(&benchOptions{proto: protoHTTP1}).(*http.Transport)
	if !ok {
		t.Fatal("http1 should use *http.Transport")
	}
	if h1.ForceAttemptHTTP2 {
		t.Error("http1 mode must not force http2")
	}

	h2, ok := newTransport(&benchOptions{proto: protoHTTP2}).(*http.Transport)
	if !ok {
		t.Fatal("http2 should also use *http.Transport")
	}
	// 设置了 TLSClientConfig 时 Go 会默认关掉 h2，必须显式打开。
	if !h2.ForceAttemptHTTP2 {
		t.Error("http2 mode must set ForceAttemptHTTP2 or it silently stays on http1")
	}

	if newTransport(&benchOptions{proto: protoHTTP3}) == nil {
		t.Fatal("http3 transport must not be nil")
	}
}
