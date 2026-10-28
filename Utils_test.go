package main

import (
	"strings"
	"testing"
)

func TestHeadersListSet(t *testing.T) {
	var h headersList

	// 旧实现返回 nil 静默丢弃，用户写错参数得不到任何提示。
	if err := h.Set("nocolon"); err == nil {
		t.Fatal("expected an error for a header without a colon")
	}
	if err := h.Set(":value"); err == nil {
		t.Fatal("expected an error for an empty header name")
	}

	if err := h.Set("sign:Random16"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := h.Set("  regid  :  123  "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(h) != 2 {
		t.Fatalf("expected 2 headers, got %d", len(h))
	}
	if h[0].key != "sign" || h[0].value != "Random16" {
		t.Fatalf("unexpected first header: %+v", h[0])
	}
	if h[1].key != "regid" || h[1].value != "123" {
		t.Fatalf("unexpected second header: %+v", h[1])
	}
}

func TestIpArraySet(t *testing.T) {
	var ips ipArray
	if err := ips.Set("   "); err == nil {
		t.Fatal("expected an error for a blank address")
	}
	if err := ips.Set(" 10.0.0.1 "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ips.Set("10.0.0.2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := ips.String(); got != "10.0.0.1,10.0.0.2" {
		t.Fatalf("unexpected join result: %q", got)
	}
}

func TestRandStringBytesMaskImpr(t *testing.T) {
	// 负数长度以前会 make([]byte, -1) panic。
	for _, n := range []int{-5, -1, 0} {
		if got := RandStringBytesMaskImpr(n); got != "" {
			t.Errorf("RandStringBytesMaskImpr(%d) = %q, want empty", n, got)
		}
	}
	for _, n := range []int{1, 12, 16, 1024} {
		got := RandStringBytesMaskImpr(n)
		if len(got) != n {
			t.Errorf("RandStringBytesMaskImpr(%d) length = %d", n, len(got))
		}
		if strings.Trim(got, letterBytes) != "" {
			t.Errorf("RandStringBytesMaskImpr(%d) produced non letter bytes: %q", n, got)
		}
	}

	// 不能是固定序列：go.mod 的 go 指令低于 1.20 时全局 rand 会被固定种子初始化。
	if a, b := RandStringBytesMaskImpr(24), RandStringBytesMaskImpr(24); a == b {
		t.Fatalf("two calls returned the same string %q, randomness looks broken", a)
	}
}

func TestRandomCount(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"Random16", 16, true},
		{"Random0", 0, true},
		{"Random", 0, false},
		{"RandomX", 0, false},
		{"Random-3", 0, false},
		{"random16", 0, false},
		{"sign", 0, false},
	}
	for _, c := range cases {
		n, ok := randomCount(c.in)
		if ok != c.ok || n != c.n {
			t.Errorf("randomCount(%q) = (%d, %v), want (%d, %v)", c.in, n, ok, c.n, c.ok)
		}
	}
}

func TestGenerateRandomIPAddress(t *testing.T) {
	for i := 0; i < 200; i++ {
		ip := generateRandomIPAddress()
		parts := strings.Split(ip, ".")
		if len(parts) != 4 {
			t.Fatalf("unexpected ip shape: %q", ip)
		}
	}
}
