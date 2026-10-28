package main

import (
	"context"
	"fmt"
	"os"
	"sync"
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

// limiter 是可动态缩小的并发闸门。
//
// 自动模式只会不断新增 worker（保持连接热着，不重建握手），
// 但"能加到多少"和"该停在多少"是两件事：实测峰值出现在 32 并发，
// 探到 64 才发现没更好，这时必须能退回 32，否则会一直跑在次优并发上。
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

var (
	autoMu       sync.Mutex
	autoOn       bool
	autoLevel    int
	autoPhase    string
	autoBest     float64
	autoBestConc int
	autoSteps    []rampStep
	fixedLevel   int
)

func setAutoState(level int, phase string) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoOn = true
	autoLevel = level
	autoPhase = phase
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

func recordStep(step rampStep) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoSteps = append(autoSteps, step)
	if len(autoSteps) > 20 {
		autoSteps = autoSteps[len(autoSteps)-20:]
	}
}

// modeSnapshot 供状态栏渲染，返回值都是拷贝，避免渲染时持锁。
func modeSnapshot() (isAuto bool, level int, phase string, best float64, bestConc int, steps []rampStep) {
	autoMu.Lock()
	defer autoMu.Unlock()
	if !autoOn {
		return false, fixedLevel, "fixed", 0, 0, nil
	}
	steps = append(steps, autoSteps...)
	return true, autoLevel, autoPhase, autoBest, autoBestConc, steps
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
	spawn := func(n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runBenchmark(ctx, opts, getTargetURL)
			}()
		}
	}

	current := start
	if current < 1 {
		current = autoStartConcurrency
	}
	if current > autoMaxConcurrency {
		current = autoMaxConcurrency
	}

	// 闸门先按起始并发卡住，之后既能放大也能缩回最优值。
	opts.gate = newLimiter(ctx, current)
	spawn(current)
	setAutoState(current, "ramping")

	prevBytes := stats.bytes.Load()
	prevFailed := stats.failed.Load()
	flat := 0
	best := 0.0
	bestConc := current

	for {
		if !waitOrDone(ctx, autoWindow) {
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
			opts.gate.SetLimit(bestConc)
			setAutoState(bestConc, "settled")
			setAutoPeak(best, bestConc)
			fmt.Fprintf(os.Stderr, "auto: settled at %d connections, peak %s/s\n",
				bestConc, readableBytes(best))
			return
		}

		if next > current {
			spawn(next - current)
			current = next
			opts.gate.SetLimit(current)
			setAutoState(current, "ramping")
		}
	}
}
