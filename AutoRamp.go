package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// 自动模式从一个保守的并发起步，然后逐级翻倍，直到吞吐不再提升。
	autoStartConcurrency = 4
	// 上限只是防止失控，正常链路远到不了。
	autoMaxConcurrency = 256
	// 单个测量窗口。太短会被握手和 TCP 慢启动污染，太长则整轮等太久。
	autoWindow = 4 * time.Second
	// 比历史峰值高出这个比例才算"还有提升"，用它抵抗链路抖动。
	autoMinGain = 0.10
	// 连续多少个"没提升"的窗口才收敛，避免被单次抖动骗过去。
	autoMaxFlatWindows = 2
	// 状态栏最多回显多少条爬坡记录。
	autoVisibleSteps = 5
)

// rampStep 记录一次测量窗口的结果，用于回显。
type rampStep struct {
	conc    int
	rate    float64 // 本窗口吞吐，字节/秒
	gain    float64 // 相对历史峰值的增幅
	hasGain bool
	failed  uint64
}

// limiter 是可动态缩放的并发闸门。
//
// worker 只增不减（保持连接热着，不重建握手），但"能加到多少"和"该停在多少"
// 是两件事：实测峰值出现在 32 并发，探到 64 才发现没更好，这时必须能退回 32。
// 多出来的 worker 只是阻塞在闸门上，不会占连接。
type limiter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	used   int
	closed bool
}

func newLimiter(ctx context.Context, limit int) *limiter {
	l := &limiter{limit: limit}
	l.cond = sync.NewCond(&l.mu)
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		l.cond.Broadcast()
	}()
	return l
}

func (l *limiter) Acquire() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for !l.closed && l.used >= l.limit {
		l.cond.Wait()
	}
	if l.closed {
		return false
	}
	l.used++
	return true
}

func (l *limiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.used--
	l.mu.Unlock()
	l.cond.Signal()
}

func (l *limiter) SetLimit(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	l.limit = n
	l.mu.Unlock()
	l.cond.Broadcast()
}

func (l *limiter) Limit() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

var (
	autoMu       sync.Mutex
	autoOn       bool
	autoLevel    int
	autoPhase    string
	autoNote     string
	autoBest     float64
	autoBestConc int
	autoSteps    []rampStep
	fixedLevel   int
	interactive  bool
)

func setAutoState(level int, phase string) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoOn = true
	autoLevel = level
	autoPhase = phase
}

func setAutoNote(note string) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoNote = note
}

func setAutoPeak(rate float64, conc int) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoBest = rate
	autoBestConc = conc
}

func setFixedConcurrency(n int) {
	autoMu.Lock()
	defer autoMu.Unlock()
	fixedLevel = n
}

func setInteractive(on bool) {
	autoMu.Lock()
	defer autoMu.Unlock()
	interactive = on
}

func recordStep(step rampStep) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoSteps = append(autoSteps, step)
	if len(autoSteps) > 20 {
		autoSteps = autoSteps[len(autoSteps)-20:]
	}
}

type modeView struct {
	isAuto bool
	level  int
	phase  string
	note   string
	best   float64
	peakAt int
	steps  []rampStep
	canKey bool
}

// modeSnapshot 供状态栏渲染，返回值都是拷贝，避免渲染时持锁。
func modeSnapshot() modeView {
	autoMu.Lock()
	defer autoMu.Unlock()
	if !autoOn {
		return modeView{level: fixedLevel, phase: "fixed", canKey: interactive}
	}
	return modeView{
		isAuto: true,
		level:  autoLevel,
		phase:  autoPhase,
		note:   autoNote,
		best:   autoBest,
		peakAt: autoBestConc,
		steps:  append([]rampStep(nil), autoSteps...),
		canKey: interactive,
	}
}

// autoRamp 负责按需增加 worker，并把闸门调到目标并发。
type autoRamp struct {
	ctx  context.Context
	opts *benchOptions
	wg   *sync.WaitGroup
	gate *limiter

	mu      sync.Mutex
	spawned int
	manual  bool
}

var activeRamp atomic.Pointer[autoRamp]

func newAutoRamp(ctx context.Context, opts *benchOptions, wg *sync.WaitGroup, start int) *autoRamp {
	return &autoRamp{
		ctx:  ctx,
		opts: opts,
		wg:   wg,
		gate: newLimiter(ctx, start),
	}
}

// setConcurrency 把有效并发调到 n：worker 不够就补，闸门同步收紧或放宽。
func (r *autoRamp) setConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > autoMaxConcurrency {
		n = autoMaxConcurrency
	}
	r.mu.Lock()
	for r.spawned < n {
		r.spawned++
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			runBenchmark(r.ctx, r.opts, getTargetURL)
		}()
	}
	r.mu.Unlock()
	r.gate.SetLimit(n)
}

func (r *autoRamp) isManual() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manual
}

// Adjust 处理用户按下的扩容/缩容键，并停止自动判定。
func (r *autoRamp) Adjust(factor float64, label string) {
	if r.ctx.Err() != nil {
		return
	}
	next := scaledConcurrency(r.gate.Limit(), factor)

	r.mu.Lock()
	r.manual = true
	r.mu.Unlock()

	r.setConcurrency(next)
	setAutoState(next, "manual")
	setAutoNote("user " + label)
}

// decideRamp 决定下一步：提升并发、原地再测一次、还是收敛。
//
// 判据是"有没有超过历史峰值 10%"，而不是"比上一个窗口高"：
// 跨境链路的单窗口抖动能到 ±30%，拿相邻窗口比会把抖动当趋势。
// 抽成纯函数是为了不启动任何 goroutine 就能把判定钉死在测试里。
func decideRamp(current, maxConc int, best, rate float64, flat int,
	windowFailed uint64, minGain float64, maxFlat int) (nextConc, nextFlat int, settle bool) {

	// 窗口里有失败，说明目标已经在拒绝负载，这时候加并发只会更糟。
	if windowFailed > 0 {
		nextFlat = flat + 1
		if nextFlat >= maxFlat {
			return current, nextFlat, true
		}
		return current, nextFlat, false
	}

	// 历史峰值未知说明这是第一个窗口，没有可比基准，直接往上试。
	if best > 0 && rate <= best*(1+minGain) {
		nextFlat = flat + 1
		if nextFlat >= maxFlat {
			return current, nextFlat, true
		}
		return current, nextFlat, false
	}

	if current >= maxConc {
		return current, 0, true
	}
	next := current * 2
	if next > maxConc {
		next = maxConc
	}
	return next, 0, false
}

// runAutoRamp 逐级提升并发直到吞吐不再超过历史峰值。
func runAutoRamp(ctx context.Context, opts *benchOptions, wg *sync.WaitGroup, start int) {
	if start < 1 {
		start = autoStartConcurrency
	}
	if start > autoMaxConcurrency {
		start = autoMaxConcurrency
	}

	ramp := newAutoRamp(ctx, opts, wg, start)
	opts.gate = ramp.gate
	// 这里刻意不在退出时清空 activeRamp：
	// 用户手动接管后自动判定会停，但按键必须继续有效，
	// 否则第二次按键就落空了。ctx 结束后 Adjust 自己会忽略输入。
	activeRamp.Store(ramp)

	ramp.setConcurrency(start)
	setAutoState(start, "ramping")

	prevBytes := stats.bytes.Load()
	prevFailed := stats.failed.Load()
	flat := 0
	best := 0.0
	bestConc := start
	current := start

	for {
		if !waitOrDone(ctx, autoWindow) {
			return
		}
		// 用户已经手动接管，自动判定就此停止，不再和用户抢控制权。
		if ramp.isManual() {
			return
		}

		curBytes := stats.bytes.Load()
		curFailed := stats.failed.Load()
		windowFailed := curFailed - prevFailed
		rate := float64(curBytes-prevBytes) / autoWindow.Seconds()

		step := rampStep{conc: current, rate: rate, failed: windowFailed}
		if best > 0 {
			step.gain = (rate - best) / best
			step.hasGain = true
		}
		recordStep(step)

		next, newFlat, settle := decideRamp(current, autoMaxConcurrency, best, rate, flat,
			windowFailed, autoMinGain, autoMaxFlatWindows)

		// 先判定再更新峰值，否则这一窗口永远"等于"自己。
		if rate > best {
			best = rate
			bestConc = current
		}
		prevBytes, prevFailed, flat = curBytes, curFailed, newFlat

		if settle {
			// 收敛到实测最优点，而不是最后试到的那个并发。
			ramp.setConcurrency(bestConc)
			setAutoState(bestConc, "settled")
			setAutoPeak(best, bestConc)
			fmt.Fprintf(os.Stderr, "auto: settled at %d connections, peak %s/s\n",
				bestConc, readableBytes(best))
			return
		}

		if next > current {
			current = next
			ramp.setConcurrency(current)
			setAutoState(current, "ramping")
		}
	}
}
