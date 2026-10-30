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
	"github.com/shirou/gopsutil/process"
)

// procStat 让人一眼分清"整机忙"和"本进程忙"。
//
// 以前状态栏只有一个 CPU 百分比，那是 cpu.Percent(0,false) 的整机数字。
// 实测踩坑：整机显示 100% 时本进程只用了 31%，剩下全是别的进程和软中断，
// 于是把"客户端 CPU 是瓶颈"当成了结论。分开显示之后，这种误判不会再发生。
type procStat struct {
	p *process.Process
}

func selfProcess() procStat {
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return procStat{}
	}
	return procStat{p: p}
}

// cpuSample 是一次整机 CPU 快照。用相邻两次的差值算占用率，
// 这样 softirq 也能单独算出来——它是网络接收的内核开销，判断客户端瓶颈时最有用。
type cpuSample struct {
	idle    float64
	softirq float64
	total   float64
}

func readCPUSample() (cpuSample, bool) {
	t, err := cpu.Times(false)
	if err != nil || len(t) == 0 {
		return cpuSample{}, false
	}
	s := t[0]
	total := s.User + s.Nice + s.System + s.Idle + s.Iowait + s.Irq + s.Softirq + s.Steal + s.Guest + s.GuestNice
	if total <= 0 {
		return cpuSample{}, false
	}
	return cpuSample{idle: s.Idle + s.Iowait, softirq: s.Softirq, total: total}, true
}

// cpuDelta 返回两次快照之间的整机占用率与 softirq 占比。
// 拿不到前后两份快照时返回 0，让状态栏显示 0 而不是瞎猜。
func cpuDelta(prev cpuSample, hadPrev bool, cur cpuSample, haveCur bool) (busy, softirq float64) {
	if !hadPrev || !haveCur {
		return 0, 0
	}
	dt := cur.total - prev.total
	if dt <= 0 {
		return 0, 0
	}
	busy = 100 * (dt - (cur.idle - prev.idle)) / dt
	softirq = 100 * (cur.softirq - prev.softirq) / dt
	if busy < 0 {
		busy = 0
	}
	if softirq < 0 {
		softirq = 0
	}
	return busy, softirq
}

// sample 返回本进程 CPU 百分比与 RSS（MB）；取不到 CPU 时返回 -1。
func (s procStat) sample() (float64, float64) {
	if s.p == nil {
		return -1, 0
	}
	cpuPct, err := s.p.CPUPercent()
	if err != nil {
		cpuPct = -1
	}
	var rssMB float64
	if mi, err := s.p.MemoryInfo(); err == nil && mi != nil {
		rssMB = float64(mi.RSS) / (1 << 20)
	}
	return cpuPct, rssMB
}

// requestStats 汇总压测结果，让 TUI 上能直接看出"到底有没有发出去"。
//
// failed 是总数，timedOut / rejected 是它的两个分桶：
//   - timedOut：我们自己秒表到点（空闲超时），目标本身没有任何问题；
//   - rejected：目标真的拒绝了（4xx/5xx）或连接层出错（拒绝、重置、半途断开）。
//
// 两者以前混在一个数字里，用户看到 Failed 在涨根本分不清是目标挂了还是 -t 设小了。
type requestStats struct {
	requests atomic.Uint64
	failed   atomic.Uint64
	rawSent  atomic.Uint64
	bytes    atomic.Uint64
	timedOut atomic.Uint64
	rejected atomic.Uint64
}

var stats requestStats

// maxVisibleNics 限制网卡表最多列几条：跑着 docker 的机器动辄十几个接口，
// 全铺出来会把状态栏刷满，而真正在跑的通常只有一两条。
const maxVisibleNics = 4

// nicRow 是网卡表的一行：本秒速率 + 累计流量。
type nicRow struct {
	name                 string
	recvTotal, sentTotal uint64
	recvRate, sentRate   uint64
}

// pickNics 按本秒流量从高到低挑出要显示的网卡，返回展示列表与被折叠的条数。
// 抽成纯函数是为了能直接测：排序错了或折叠漏了，状态栏会悄悄骗人。
func pickNics(rows []nicRow) (shown []nicRow, quiet int) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].recvRate+rows[i].sentRate > rows[j].recvRate+rows[j].sentRate
	})
	for _, r := range rows {
		if r.recvRate+r.sentRate == 0 || len(shown) >= maxVisibleNics {
			quiet++
			continue
		}
		shown = append(shown, r)
	}
	return shown, quiet
}

// middleEllipsis 把超长接口名压到固定宽度。
// 取中间省略而不是砍尾巴：veth/br 的名字前缀都一样，区别全在后半段。
func middleEllipsis(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return s[:left] + "..." + s[len(s)-right:]
}

// recordFailure 记一次失败并分桶。reqCtx 是带了空闲看门的请求 ctx，
// 用来判断这次的取消是不是我们自己掐的。
func (s *requestStats) recordFailure(err error, reqCtx context.Context) {
	s.failed.Add(1)
	if isTimeoutError(err, reqCtx) {
		s.timedOut.Add(1)
		return
	}
	s.rejected.Add(1)
}

// recordStatusCode 记一次"目标明确拒绝"（4xx/5xx）。
// 这类响应以前被当作成功计入 requests，压测结果因此可以全绿而实际全在报错。
func (s *requestStats) recordStatusCode() {
	s.failed.Add(1)
	s.rejected.Add(1)
}

// failureBreakdown 给状态栏拼出失败原因明细，没有明细时返回空串。
func failureBreakdown() string {
	parts := make([]string, 0, 2)
	if n := stats.timedOut.Load(); n > 0 {
		parts = append(parts, fmt.Sprintf("timeout:%d", n))
	}
	if n := stats.rejected.Load(); n > 0 {
		parts = append(parts, fmt.Sprintf("rejected:%d", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ",") + ")"
}

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
	self := selfProcess()
	prevLoaded := uint64(0)
	lastSample := started
	prevCPU, havePrevCPU := readCPUSample()

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

		curCPU, haveCPU := readCPUSample()
		memStat, _ := mem.VirtualMemory()
		loadStat, _ := load.Avg()
		cur := snapshotNetCounters()

		view := modeSnapshot()
		spoof := ""
		if *proxySpoof {
			spoof = "   spoof:proxy"
		}

		// 统一成"标签列 + 值列"的两段式，标签固定 7 列宽，值都在同一竖线上。
		row := func(name, value string) { line("%-7s %s", name, value) }

		row("Target", shortText(getTargetURL(), 92))
		if ipList != "" {
			row("IP", shortText(ipList, 92))
		}

		switch {
		case !view.isAuto:
			row("Mode", fmt.Sprintf("fixed   conc %d%s", view.level, spoof))
		case view.phase == "settled" && view.best > 0:
			row("Mode", fmt.Sprintf("auto settled   conc %d   peak %s/s @%d%s",
				view.level, readableBytes(view.best), view.peakAt, spoof))
		case view.phase == "refine" && view.high > 0:
			// 收敛过程中把区间亮出来，用户能看出还差多少档。
			row("Mode", fmt.Sprintf("auto refine   conc %d   bracket %d..%d%s",
				view.level, view.low, view.high, spoof))
		case view.note != "":
			row("Mode", fmt.Sprintf("auto %s   conc %d   (%s)%s", view.phase, view.level, view.note, spoof))
		default:
			row("Mode", fmt.Sprintf("auto %s   conc %d%s", view.phase, view.level, spoof))
		}

		loaded := stats.bytes.Load()
		elapsed := time.Since(started).Seconds()
		nowRate := 0.0
		if d := time.Since(lastSample).Seconds(); d > 0 {
			nowRate = float64(loaded-prevLoaded) / d
		}
		prevLoaded, lastSample = loaded, time.Now()
		var avg float64
		if elapsed > 0 {
			avg = float64(loaded) / elapsed
		}

		row("Rate", fmt.Sprintf("avg %s/s   now %s/s", readableBytes(avg), readableBytes(nowRate)))
		// raw 模式不解析响应，没有"完成的请求"，只有"发出去的请求"。
		countText := fmt.Sprintf("%d req", stats.requests.Load())
		if *rawMode {
			countText = fmt.Sprintf("%d sent", stats.rawSent.Load())
		}
		row("Volume", fmt.Sprintf("%s   %s   %d failed%s   %.0fs",
			readableBytes(float64(loaded)), countText, stats.failed.Load(),
			failureBreakdown(), elapsed))

		if msg := lastErrorMessage(); msg != "" {
			row("Error", shortText(msg, 100))
		}

		// 两个 CPU 数字并排显示：只说"整机 100%"会让人以为是压测进程打满了，
		// 实测踩过这个坑——整机满载时本进程只用了 31%，瓶颈其实在别处。
		procCPU, procRSS := self.sample()
		busyPct, softPct := cpuDelta(prevCPU, havePrevCPU, curCPU, haveCPU)
		prevCPU, havePrevCPU = curCPU, haveCPU
		cpuText := fmt.Sprintf("%.1f%% proc / %.1f%% busy", procCPU, busyPct)
		if procCPU < 0 {
			cpuText = fmt.Sprintf("n/a proc / %.1f%% busy", busyPct)
		}
		// softirq 是网络接收的内核开销，它才是"客户端跑不动"时的大头。
		if softPct > 0.05 {
			cpuText += fmt.Sprintf("   softirq %.1f%%", softPct)
		}
		if loadStat != nil {
			cpuText += fmt.Sprintf("   load %.2f %.2f %.2f", loadStat.Load1, loadStat.Load5, loadStat.Load15)
		}
		row("CPU", cpuText)
		if memStat != nil {
			memText := fmt.Sprintf("%.1fMB rss / %.1f%% sys", procRSS, memStat.UsedPercent)
			// swap 用了一半以上的机器，吞吐会被换页拖垮；这行不显示就永远看不出来。
			if swapStat, err := mem.SwapMemory(); err == nil && swapStat != nil && swapStat.Total > 0 && swapStat.Used > 0 {
				memText += fmt.Sprintf("   swap %s used", readableBytes(float64(swapStat.Used)))
			}
			row("Mem", memText)
		}

		if len(view.steps) > 0 {
			steps := view.steps
			if len(steps) > autoVisibleSteps {
				steps = steps[len(steps)-autoVisibleSteps:]
			}
			row("Ramp", fmt.Sprintf("%4s %11s %10s %9s", "conc", "rate", "per-conn", "vs peak"))
			for i, s := range steps {
				change := "     -"
				if s.hasGain {
					change = fmt.Sprintf("%+6.1f%%", s.gain*100)
				}
				mark := ""
				if i == len(steps)-1 && view.phase == "settled" {
					mark = "   plateau"
				}
				if s.failed > 0 {
					mark = fmt.Sprintf("   failed:%d", s.failed)
				}
				// 每连接速率：并发翻倍而它减半，说明总带宽被目标按 IP 卡住了。
				perConn := 0.0
				if s.conc > 0 {
					perConn = s.rate / float64(s.conc)
				}
				line("        %4d %11s %10s %9s%s",
					s.conc, readableBytes(s.rate)+"/s", readableBytes(perConn)+"/s", change, mark)
			}
		}

		// 网卡按"本秒流量"排序，只列真正在动的几条；几十条 docker/veth 不再刷屏。
		row("Net", fmt.Sprintf("%-12s %10s %10s   %s", "iface", "down/s", "up/s", "total down/up"))
		rows := make([]nicRow, 0, len(cur))
		for _, name := range sortedNames(cur) {
			c := cur[name]
			var rr, sr uint64
			if p, ok := prev[name]; ok && c.recv >= p.recv && c.sent >= p.sent {
				rr, sr = c.recv-p.recv, c.sent-p.sent
			}
			rows = append(rows, nicRow{name: name, recvTotal: c.recv, sentTotal: c.sent, recvRate: rr, sentRate: sr})
		}
		shownNics, quiet := pickNics(rows)
		for _, r := range shownNics {
			line("        %-12s %10s %10s   %s/%s",
				middleEllipsis(r.name, 12),
				readableBytes(float64(r.recvRate))+"/s", readableBytes(float64(r.sentRate))+"/s",
				readableBytes(float64(r.recvTotal)), readableBytes(float64(r.sentTotal)))
		}
		if quiet > 0 {
			line("        %-12s %10s %10s   (%d quiet)", "(others)", "", "", quiet)
		}

		if view.canKey {
			row("Keys", concurrencyLegend())
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
