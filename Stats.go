package main

import (
	"context"
	"fmt"
	"math"
	URL "net/url"
	"os"
	"sort"
	"strings"
	"sync"
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

// 失败原因以前只进计数器，用户看到 Failed 在涨却不知道原因
// （比如用 -6 去测一个只有 IPv4 的目标）。这里把每种错误打印一次，
// 并把最后一条挂到状态栏上。
var (
	errMu     sync.Mutex
	seenError = make(map[string]struct{})
	lastError string
)

const maxDistinctErrors = 20

func reportError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()

	errMu.Lock()
	_, seen := seenError[msg]
	if !seen && len(seenError) < maxDistinctErrors {
		seenError[msg] = struct{}{}
	}
	lastError = msg
	errMu.Unlock()

	if !seen {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
}

func lastErrorMessage() string {
	errMu.Lock()
	defer errMu.Unlock()
	return lastError
}

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

	// raw 模式下终端的 OPOST 会被关掉，"\n" 不再自动补回车，
	// 所以这里统一显式写 "\r\n"，有没有 OPOST 都不会出现阶梯错位。
	line := func(format string, args ...interface{}) {
		fmt.Fprintf(TerminalWriter, format+"\r\n", args...)
	}

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

		line("URL:%s", getTargetURL())
		if ipList != "" {
			line("IP:%s", ipList)
		}

		view := modeSnapshot()
		switch {
		case !view.isAuto:
			line("Mode:fixed  Conc:%d", view.level)
		case view.phase == "settled" && view.best > 0:
			line("Mode:auto  Phase:settled  Conc:%d  Peak:%s/s@%d",
				view.level, readableBytes(view.best), view.peakAt)
		case view.note != "":
			line("Mode:auto  Phase:%s  Conc:%d  (%s)", view.phase, view.level, view.note)
		default:
			line("Mode:auto  Phase:%s  Conc:%d", view.phase, view.level)
		}

		loaded := stats.bytes.Load()
		elapsed := time.Since(started).Seconds()
		var avg float64
		if elapsed > 0 {
			avg = float64(loaded) / elapsed
		}
		line("Requests:%d  Failed:%d  Loaded:%s  Avg:%s/s  Elapsed:%ds",
			stats.requests.Load(), stats.failed.Load(),
			readableBytes(float64(loaded)), readableBytes(avg), int(elapsed))

		if msg := lastErrorMessage(); msg != "" {
			line("LastError:%s", shortText(msg, 110))
		}

		if view.canKey {
			line("Keys:%s", concurrencyLegend())
		}

		if len(view.steps) > 0 {
			line("Ramp:")
			steps := view.steps
			if len(steps) > autoVisibleSteps {
				steps = steps[len(steps)-autoVisibleSteps:]
			}
			for i, s := range steps {
				change := "      -"
				if s.hasGain {
					change = fmt.Sprintf("%+6.1f%%", s.gain*100)
				}
				mark := ""
				if i == len(steps)-1 && view.phase == "settled" {
					mark = "  <- plateau"
				}
				if s.failed > 0 {
					mark = fmt.Sprintf("  [failed:%d]", s.failed)
				}
				line("  conc=%-4d %10s/s %s%s", s.conc, readableBytes(s.rate), change, mark)
			}
		}

		line("CPU:%.2f%% ", firstPercent(percent))
		if memStat != nil {
			line("Memory:%.3f%% ", memStat.UsedPercent)
		}
		if loadStat != nil {
			line("Load:%.3f %.3f %.3f", loadStat.Load1, loadStat.Load5, loadStat.Load15)
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
			line("Nic:%v,Recv %s(%s/s),Send %s(%s/s)", name,
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

// shortText 把可能带换行的错误压成一行并截断，避免撑坏状态栏。
func shortText(s string, limit int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
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
// 否则对当前目标做一次地址查询，记录类型跟随地址族。
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
