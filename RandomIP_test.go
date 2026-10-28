package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// 回归 P2-4：非法输入必须返回错误，而不是 nil 解引用崩掉整个进程。
func TestGenerateRandomIPFromCIDRRejectsInvalid(t *testing.T) {
	bad := []string{
		"",
		"#comment",
		"not-a-cidr",
		"1.2.3.4",        // 缺掩码
		"2001:db8::/32",  // IPv6
		"203.0.112.0/33", // 掩码越界
	}
	for _, in := range bad {
		if _, err := GenerateRandomIPFromCIDR(in); err == nil {
			t.Errorf("GenerateRandomIPFromCIDR(%q) should fail", in)
		}
	}
}

func TestGenerateRandomIPFromCIDRStaysInRange(t *testing.T) {
	const cidr = "203.0.112.0/20"
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		out, err := GenerateRandomIPFromCIDR(cidr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ip := net.ParseIP(out)
		if ip == nil {
			t.Fatalf("generated %q which is not an ip", out)
		}
		if !ipNet.Contains(ip) {
			t.Fatalf("generated %s outside %s", out, cidr)
		}
	}
}

// 回归 P2-5：非 8 位对齐的掩码（/20）也必须覆盖整个网段。
// 旧实现用 mask[i]==0 判断，只会随机最后一个字节，整个 /20 被压缩成 /24。
func TestGenerateRandomIPFromCIDRCoversWholeSubnet(t *testing.T) {
	const cidr = "203.0.112.0/20"
	third := map[string]bool{}
	last := map[string]bool{}

	for i := 0; i < 600; i++ {
		out, err := GenerateRandomIPFromCIDR(cidr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parts := strings.Split(out, ".")
		if len(parts) != 4 {
			t.Fatalf("unexpected ip: %q", out)
		}
		third[parts[2]] = true
		last[parts[3]] = true
	}

	// /20 的第三段应覆盖 112..127 共 16 个取值。
	if len(third) < 10 {
		t.Fatalf("third octet only took %d values (%v), the subnet is not being sampled", len(third), keysOf(third))
	}
	if len(last) < 100 {
		t.Fatalf("last octet only took %d values, sampling looks broken", len(last))
	}
}

func TestGenerateRandomIPFromCIDRSlash32(t *testing.T) {
	out, err := GenerateRandomIPFromCIDR("203.0.113.9/32")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "203.0.113.9" {
		t.Fatalf("a /32 must always resolve to its only address, got %q", out)
	}
}

func TestRemoveEmptyItem(t *testing.T) {
	got := RemoveEmptyItem([]string{"a", "", "b", ""})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected result: %v", got)
	}
	if got := RemoveEmptyItem(nil); len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func withCIDRList(t *testing.T, list []string) {
	t.Helper()
	cidrMu.Lock()
	old := cidrList
	cidrList = list
	cidrMu.Unlock()
	t.Cleanup(func() {
		cidrMu.Lock()
		cidrList = old
		cidrMu.Unlock()
	})
}

// 回归：随机 IP 生成绝不能碰网络。
// 旧实现把下载放在请求路径上，下载慢时所有 goroutine 一起堵住，
// 实测默认参数下 4 秒 0 请求。
func TestRandomForwardedIPNeverBlocks(t *testing.T) {
	withCIDRList(t, nil)

	done := make(chan string, 1)
	go func() { done <- randomForwardedIP() }()

	select {
	case ip := <-done:
		if net.ParseIP(ip) == nil {
			t.Fatalf("fallback returned %q which is not an ip", ip)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("randomForwardedIP blocked, it must never touch the network")
	}
}

func TestRandomForwardedIPUsesLoadedList(t *testing.T) {
	const cidr = "203.0.113.0/24"
	withCIDRList(t, []string{cidr})

	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		ip := net.ParseIP(randomForwardedIP())
		if ip == nil {
			t.Fatal("generated an invalid ip")
		}
		if !ipNet.Contains(ip) {
			t.Fatalf("generated %s outside the loaded list %s", ip, cidr)
		}
	}
}
