package main

import (
	"math"
	"testing"
)

// 回归 P2-1 / P2-2 / readableBytes 越界：负数、小数、NaN 都必须安全。
func TestReadableBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0B"},
		{-1, "0B"},
		{-1024, "0B"},
		{0.5, "0.500B"},
		{1, "1.000B"},
		{1024, "1.000KB"},
		{1024 * 1024, "1.000MB"},
		{1024 * 1024 * 1024, "1.000GB"},
	}
	for _, c := range cases {
		if got := readableBytes(c.in); got != c.want {
			t.Errorf("readableBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}

	// NaN / +Inf 以前会算出离谱下标直接 panic。
	if got := readableBytes(math.NaN()); got != "0B" {
		t.Errorf("readableBytes(NaN) = %q, want 0B", got)
	}
	if got := readableBytes(math.Inf(1)); got != "0B" {
		t.Errorf("readableBytes(+Inf) = %q, want 0B", got)
	}
}

func TestSnapshotNetCountersIsUsable(t *testing.T) {
	// 只要求不 panic、返回可用的 map；网卡内容是环境相关的。
	counters := snapshotNetCounters()
	if counters == nil {
		t.Fatal("snapshotNetCounters returned nil, want an empty map at worst")
	}
	for name, c := range counters {
		if name == "" {
			t.Error("found a nic with an empty name")
		}
		_ = c
	}
}

func TestSortedNames(t *testing.T) {
	got := sortedNames(map[string]netCounters{
		"eth0":  {},
		"Loop":  {},
		"wlan0": {},
	})
	want := []string{"Loop", "eth0", "wlan0"}
	if len(got) != len(want) {
		t.Fatalf("unexpected length: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedNames = %v, want %v", got, want)
		}
	}
}

// 网卡表只该显示真正在动的几条，且按本秒流量从高到低排。
func TestPickNicsSortsAndFoldsIdle(t *testing.T) {
	rows := []nicRow{
		{name: "br-07d13f564ff4", recvRate: 0},
		{name: "eth0", recvRate: 9 << 20, sentRate: 1 << 20},
		{name: "he-ipv6", recvRate: 0, recvTotal: 47 << 20},
		{name: "lo", recvRate: 2 << 20},
		{name: "veth2099e26", recvRate: 0},
	}

	shown, quiet := pickNics(rows)
	if len(shown) != 2 {
		t.Fatalf("shown = %d, want the 2 moving nics", len(shown))
	}
	if shown[0].name != "eth0" || shown[1].name != "lo" {
		t.Fatalf("shown order = %v, want eth0 then lo by rate", []string{shown[0].name, shown[1].name})
	}
	// 三条全零速率的接口必须被折叠掉，否则状态栏又被它们占满。
	if quiet != 3 {
		t.Fatalf("quiet = %d, want 3 folded interfaces", quiet)
	}
}

// 活跃接口多于上限时只留最快的几条，其余计入折叠。
func TestPickNicsCapsVisibleRows(t *testing.T) {
	rows := make([]nicRow, 0, 6)
	for i := 0; i < 6; i++ {
		rows = append(rows, nicRow{name: string(rune('a' + i)), recvRate: uint64(i+1) << 20})
	}
	shown, quiet := pickNics(rows)
	if len(shown) != maxVisibleNics {
		t.Fatalf("shown = %d, want the cap %d", len(shown), maxVisibleNics)
	}
	if quiet != 6-maxVisibleNics {
		t.Fatalf("quiet = %d, want %d", quiet, 6-maxVisibleNics)
	}
	if shown[0].recvRate != 6<<20 {
		t.Fatalf("fastest nic should come first, got %d", shown[0].recvRate)
	}
}

func TestMiddleEllipsisKeepsWidth(t *testing.T) {
	cases := []struct {
		in    string
		width int
	}{
		{"eth0", 12},
		{"br-07d13f564ff4", 12},
		{"vetha5e4fc7", 12},
		{"a-very-long-interface-name", 9},
	}
	for _, c := range cases {
		got := middleEllipsis(c.in, c.width)
		if len(got) > c.width {
			t.Errorf("middleEllipsis(%q,%d) = %q, length %d exceeds width", c.in, c.width, got, len(got))
		}
		// 短名字必须原样返回，不能凭空截断。
		if len(c.in) <= c.width && got != c.in {
			t.Errorf("middleEllipsis(%q,%d) = %q, want it unchanged", c.in, c.width, got)
		}
	}
	// 折叠点必须在中间，保留结尾的区分部分。
	if got := middleEllipsis("br-07d13f564ff4", 12); got[len(got)-3:] == "..." {
		t.Errorf("middleEllipsis should keep the tail, got %q", got)
	}
}
