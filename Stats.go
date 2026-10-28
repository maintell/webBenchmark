package main

import (
	"context"
	"fmt"
	"math"
	URL "net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	netstat "github.com/shirou/gopsutil/net"
)

// requestStats 汇总压测结果，让 TUI 上能直接看出"到底有没有发出去"。
type requestStats struct {
	requests atomic.Uint64
	failed   atomic.Uint64
	bytes    atomic.Uint64
}

var stats requestStats

type netCounters struct {
	recv, sent uint64
}

// snapshotNetCounters 以网卡名索引。以前是按切片下标对齐两次快照，
// 网卡一增减（拨 VPN、插网卡）就会越界 panic 把整个进程带走。
func snapshotNetCounters() map[string]netCounters {
	out := make(map[string]netCounters)
	list, err := netstat.IOCounters(true)
	if err != nil {
		return out
	}
	for _, c := range list {
		out[c.Name] = netCounters{recv: c.BytesRecv, sent: c.BytesSent}
	}
	return out
}

func showStat(ctx context.Context) {
	started := time.Now()
	prev := snapshotNetCounters()
	ipList := resolveTargetIPs()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		percent, _ := cpu.Percent(0, false)
		memStat, _ := mem.VirtualMemory()
		loadStat, _ := load.Avg()
		cur := snapshotNetCounters()

		fmt.Fprintf(TerminalWriter, "URL:%s\n", getTargetURL())
		if ipList != "" {
			fmt.Fprintf(TerminalWriter, "IP:%s\n", ipList)
		}
		loaded := stats.bytes.Load()
		elapsed := time.Since(started).Seconds()
		var avg float64
		if elapsed > 0 {
			avg = float64(loaded) / elapsed
		}
		fmt.Fprintf(TerminalWriter, "Requests:%d  Failed:%d  Loaded:%s  Avg:%s/s  Elapsed:%ds\n",
			stats.requests.Load(), stats.failed.Load(),
			readableBytes(float64(loaded)), readableBytes(avg), int(elapsed))
		fmt.Fprintf(TerminalWriter, "CPU:%.2f%% \n", firstPercent(percent))
		if memStat != nil {
			fmt.Fprintf(TerminalWriter, "Memory:%.3f%% \n", memStat.UsedPercent)
		}
		if loadStat != nil {
			fmt.Fprintf(TerminalWriter, "Load:%.3f %.3f %.3f\n", loadStat.Load1, loadStat.Load5, loadStat.Load15)
		}
		for _, name := range sortedNames(cur) {
			c := cur[name]
			if c.recv == 0 && c.sent == 0 {
				continue
			}
			var recvRate, sentRate uint64
			if p, ok := prev[name]; ok && c.recv >= p.recv && c.sent >= p.sent {
				recvRate = c.recv - p.recv
				sentRate = c.sent - p.sent
			}
			fmt.Fprintf(TerminalWriter, "Nic:%v,Recv %s(%s/s),Send %s(%s/s)\n", name,
				readableBytes(float64(c.recv)),
				readableBytes(float64(recvRate)),
				readableBytes(float64(c.sent)),
				readableBytes(float64(sentRate)))
		}
		prev = cur

		TerminalWriter.Clear()
		TerminalWriter.Print()
	}
}

func firstPercent(percent []float64) float64 {
	if len(percent) == 0 {
		return 0
	}
	return percent[0]
}

func sortedNames(counters map[string]netCounters) []string {
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveTargetIPs 返回用于展示的目标地址：指定了 -i 就展示这些 IP，
// 否则对当前目标做一次 A 记录查询。-sub 模式下也能拿到正确的域名。
func resolveTargetIPs() string {
	if len(customIP) > 0 {
		return customIP.String()
	}
	u, err := URL.Parse(getTargetURL())
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.Join(nslookup(u.Hostname(), *dnsServer), ",")
}

func readableBytes(bytes float64) (expression string) {
	sizes := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}
	if bytes <= 0 || math.IsNaN(bytes) || math.IsInf(bytes, 0) {
		return "0B"
	}
	i := int(math.Floor(math.Log(bytes) / math.Log(1024)))
	// 小于 1 的值会算出负数下标，NaN/Inf 会算出难以置信的下标，都必须夹住。
	if i < 0 {
		i = 0
	}
	if i >= len(sizes) {
		i = len(sizes) - 1
	}
	return fmt.Sprintf("%.3f%s", bytes/math.Pow(1024, float64(i)), sizes[i])
}
