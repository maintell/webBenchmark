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
