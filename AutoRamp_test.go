package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAutoConcurrency(t *testing.T) {
	old := *count
	defer func() { *count = old }()

	cases := []struct {
		count int
		want  bool
	}{
		{0, true},  // 默认值：自动
		{-1, true}, // 负数没有意义，交给 validateFlags 拦下，这里同样按自动
		{1, false},
		{16, false},
		{256, false},
	}
	for _, c := range cases {
		*count = c.count
		if got := autoConcurrency(); got != c.want {
			t.Errorf("autoConcurrency(count=%d) = %v, want %v", c.count, got, c.want)
		}
	}
}

// 第一个窗口没有历史峰值，必须直接往上试，否则永远停在起始并发。
func TestRampFirstWindowGrows(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(4)

	act := st.observe(cfg, 1_000_000, 0)
	if act.settle {
		t.Fatal("the first window must not settle")
	}
	if act.level != 6 {
		t.Fatalf("4 with step %.2f should become 6, got %d", cfg.step, act.level)
	}
}

// 步进必须是几何增长而不是翻倍：翻倍会直接从 16 跳到 32，
// 把 22 这种真峰整个跨过去。
func TestRampGrowthIsGeometricNotDoubling(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(16)

	act := st.observe(cfg, 1_000_000, 0)
	if act.level != 24 {
		t.Fatalf("16 with step %.2f should become 24, got %d", cfg.step, act.level)
	}
}

func TestRampStepIsConfigurable(t *testing.T) {
	cfg := (&benchOptions{rampStep: 1.2}).rampConfig()
	st := newRampState(10)

	if act := st.observe(cfg, 1, 0); act.level != 12 {
		t.Fatalf("10 with step 1.2 should become 12, got %d", act.level)
	}
}

// 死区：只比峰值高 5% 不算提升，要原地复测，而不是继续加并发。
func TestRampDeadZoneHoldsInsteadOfGrowing(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(16)
	st.observe(cfg, 10_000_000, 0) // 峰值 10MB/s@16，下一档 24

	act := st.observe(cfg, 10_500_000, 0) // 24 档只有 +5%
	if act.settle {
		t.Fatal("one flat window must not settle yet")
	}
	if act.level != 24 {
		t.Fatalf("a flat window should re-measure the same level 24, got %d", act.level)
	}
	if act.phase != rampClimb {
		t.Fatalf("phase = %v, want climb", act.phase)
	}
}

// 连续两个窗口没提升就转入精细收敛：先做一次突破探测，探测失败才拿
// [峰值档, 已确认更差的档] 作区间取几何中点。
func TestRampRefinesBetweenPeakAndFailingLevel(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(16)
	st.observe(cfg, 10_000_000, 0) // 峰值 10MB/s@16 → 当前 24
	st.observe(cfg, 9_500_000, 0)  // 24 档没超过峰值 → flat=1，原地复测

	act := st.observe(cfg, 9_500_000, 0) // flat=2 → 突破探测
	if act.phase != rampClimb || act.level != 36 {
		t.Fatalf("a plateau should first probe one step higher (36), got level=%d phase=%v", act.level, act.phase)
	}

	act = st.observe(cfg, 9_500_000, 0) // 探测也失败 → 进入收敛
	if act.phase != rampRefine {
		t.Fatalf("phase = %v, want refine", act.phase)
	}
	if act.level != 24 { // sqrt(16*36) = 24
		t.Fatalf("geometric midpoint of [16,36] should be 24, got %d", act.level)
	}
}

// 平台后面还有更高的峰时，突破探测必须把爬坡拉回来。
// 实测就是这么被坑的：6 并发 48.8MB/s、9 并发两个窗口都只有 48.5，
// 于是一路收敛到 6，而同一台机器 8 并发能跑到 62.6MB/s。
func TestRampBreakoutRecoversFromLocalDip(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(6)

	st.observe(cfg, 48_000_000, 0)        // 6 档：48MB/s，下一档 9
	st.observe(cfg, 40_000_000, 0)        // 9 档果然是低谷 → flat=1
	act := st.observe(cfg, 40_000_000, 0) // flat=2 → 突破探测
	if act.level != 14 {
		t.Fatalf("breakout probe should jump to 14, got %d", act.level)
	}

	act = st.observe(cfg, 62_000_000, 0) // 14 档 62MB/s，远超峰值 10% 门槛
	if act.settle {
		t.Fatal("a level that beats the peak must resume climbing, not settle")
	}
	if act.level <= 14 {
		t.Fatalf("climb should continue above 14, got %d", act.level)
	}
}

// 观测到的最高吞吐必须成为最终答案，哪怕它没跨过爬坡死区。
// 回归：某次 9 并发 68.0MB/s、14 并发 72.1MB/s（+6%，在死区里），
// 旧写法把 72.1 扔掉、收敛到 9，白丢 6% 的实测最优。
func TestRampAdoptsHigherObservationInsideDeadZone(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(9)
	st.observe(cfg, 68_000_000, 0) // 9 档 68MB/s → 下一档 14

	act := st.observe(cfg, 72_000_000, 0) // 14 档 +5.9%，不够驱动爬坡
	if act.settle {
		t.Fatal("a 5.9% gain must not settle")
	}
	if act.level != 14 {
		t.Fatalf("under the dead zone the level should be re-measured at 14, got %d", act.level)
	}
	if st.bestConc != 14 || st.best != 72_000_000 {
		t.Fatalf("the highest observation must become the peak, got %.0f@%d", st.best, st.bestConc)
	}

	// 之后无论怎么试，收敛都必须回到这个实测最高的档位。
	for i := 0; i < 40; i++ {
		act = st.observe(cfg, 66_000_000, 0)
		if act.settle {
			if act.level != 14 {
				t.Fatalf("settled at %d, want the level that measured highest (14)", act.level)
			}
			return
		}
	}
	t.Fatal("ramp never settled")
}

// 突破探测只要跑出任何新高就必须继续爬。
// 实测：4 档 41.1MB/s，6 档连测两窗都是 27.6（低谷），9 档 43.2——
// 只高 5.3%，若按 10% 死区就白白停在 9，永远不知道 14 档能跑多少。
func TestRampBreakoutResumesOnAnyNewHigh(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(4)
	st.observe(cfg, 41_000_000, 0)        // 4 档 41MB/s → 下一档 6
	st.observe(cfg, 28_700_000, 0)        // 6 档低谷 → flat=1
	act := st.observe(cfg, 27_600_000, 0) // flat=2 → 突破探测
	if act.level != 9 {
		t.Fatalf("breakout probe should jump from 6 to 9, got %d", act.level)
	}

	act = st.observe(cfg, 43_200_000, 0) // 9 档新高，但只有 +5.3%
	if act.settle {
		t.Fatal("a breakout window that sets a new high must not settle")
	}
	if act.level <= 9 {
		t.Fatalf("climb must continue above 9 after the breakout, got %d", act.level)
	}
}

// 收敛前必须做一次相邻档对决：现役与上一档各测同样多的窗口再定胜负。
// 单窗在跨境链路上能抖 ±20%，实测就有 9 档单窗 61.4、14 档两窗 68.1/72.1 却停在 9 的情况。
func TestRampDuelKeepsIncumbentWhenChallengerIsWorse(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(9)
	st.best, st.bestConc, st.low, st.high = 100_000_000, 9, 9, 14

	act := st.settle(cfg)
	if act.phase != rampDuel || act.level != 9 {
		t.Fatalf("settling should first re-measure the incumbent, got level=%d phase=%v", act.level, act.phase)
	}

	st.observe(cfg, 100_000_000, 0)       // 现役第 1 窗
	act = st.observe(cfg, 100_000_000, 0) // 现役第 2 窗 → 转去测挑战者
	if act.phase != rampDuel || act.level != 14 {
		t.Fatalf("after confirming the incumbent it should probe 14, got level=%d phase=%v", act.level, act.phase)
	}

	st.observe(cfg, 95_000_000, 0)       // 挑战者第 1 窗
	act = st.observe(cfg, 95_000_000, 0) // 挑战者第 2 窗
	if !act.settle {
		t.Fatal("the duel must settle after both sides are measured")
	}
	if act.level != 9 {
		t.Fatalf("a slower challenger must not take over, settled at %d", act.level)
	}
	// 峰值要用复核后的均值，不能留着单窗那个偏高的数字。
	if st.best != 100_000_000 {
		t.Fatalf("best = %.0f, want the confirmed mean", st.best)
	}
}

func TestRampDuelMovesUpWhenChallengerWins(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(9)
	st.best, st.bestConc, st.low, st.high = 61_000_000, 9, 9, 21

	st.settle(cfg)
	st.observe(cfg, 61_000_000, 0)
	st.observe(cfg, 61_000_000, 0)
	st.observe(cfg, 70_000_000, 0)
	act := st.observe(cfg, 70_000_000, 0)

	if !act.settle {
		t.Fatal("the duel must settle")
	}
	if act.level != 14 {
		t.Fatalf("a clearly faster challenger must take over, settled at %d", act.level)
	}
	if st.best != 70_000_000 {
		t.Fatalf("best = %.0f, want the challenger mean", st.best)
	}
}

// -ramp-duel=false 时直接收工，不再花窗口复核。
func TestRampDuelCanBeDisabled(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	cfg.duel = false
	st := newRampState(9)
	st.best, st.bestConc = 100_000_000, 9

	act := st.settle(cfg)
	if !act.settle || act.phase != rampSettled {
		t.Fatalf("with the duel disabled it should settle immediately, got phase=%v settle=%v", act.phase, act.settle)
	}
	if act.level != 9 {
		t.Fatalf("settled at %d, want 9", act.level)
	}
}

// 抖动大的链路：噪声可以刷新"最高观测"，但不许驱动爬坡。
// 分界线是——峰值决定最终停在哪一档，死区决定还要不要继续加并发。
func TestRampNoiseRaisesDeadZone(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(16)
	st.observe(cfg, 10_000_000, 0) // 峰值 10MB/s@16 → 当前 24
	st.observe(cfg, 9_000_000, 0)  // 24 档第一次，flat=1

	// 同一档再来一个差 25% 的窗口：抖动能被量出来，12% 的"提升"不该驱动爬坡。
	act := st.observe(cfg, 12_000_000, 0)
	if st.noise <= 0.2 {
		t.Fatalf("noise = %.3f, want the 3 MB/s spread to be detected", st.noise)
	}
	if gate := st.gate(cfg); gate <= cfg.minGain {
		t.Fatalf("gate = %.3f, want it raised above the %.2f dead zone", gate, cfg.minGain)
	}
	// 12% < 25% 的门槛：不许当成"还能更高"，于是只加一次 flat，不进入爬坡。
	if act.phase == rampClimb && act.level > 24 && st.flat == 0 {
		t.Fatalf("a 12%% bump under 25%% noise must not drive a climb step, got level=%d", act.level)
	}
}

// 失败窗口说明这一档已经不可用，绝不能继续往上加。
func TestRampFailuresBlockGrowth(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(8)

	act := st.observe(cfg, 5_000_000, 3)
	if act.settle {
		t.Fatal("a single window with failures should not settle immediately")
	}
	if act.level != 8 {
		t.Fatalf("must not raise concurrency while requests fail, got %d", act.level)
	}
}

// 一字节都不进来的链路必须收敛在起始档，而不是一路翻到 256。
func TestRampZeroThroughputSettles(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(4)

	for i := 0; i < 50; i++ {
		act := st.observe(cfg, 0, 0)
		if !act.settle {
			continue
		}
		if act.level != 4 {
			t.Fatalf("settled at %d, want the starting level 4", act.level)
		}
		return
	}
	t.Fatal("zero throughput must settle instead of ramping to the cap")
}

func TestRampStopsAtMaxConcurrency(t *testing.T) {
	cfg := (&benchOptions{}).rampConfig()
	st := newRampState(autoMaxConcurrency)

	act := st.observe(cfg, 1_000_000, 0)
	if !act.settle {
		t.Fatalf("must settle when the cap cannot be exceeded, got level=%d", act.level)
	}
}

// 分辨率的核心回归：真峰在 22 时，旧的"翻倍 + 10% 死区"只会停在 16，
// 白白丢掉约 27% 的吞吐。新算法必须落到 ±1 以内。
func TestRampSettlesNearTrueOptimum(t *testing.T) {
	cases := []struct {
		name    string
		optimal int
	}{
		{"peak at 22", 22},
		{"peak at 16", 16},
		{"peak at 37", 37},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := (&benchOptions{}).rampConfig()
			optimal := c.optimal
			// 峰前每加一条并发都更快，过峰后互相拖累，越挤越慢。
			curve := func(conc int) float64 {
				if conc <= optimal {
					return float64(conc) * 1_000_000
				}
				return float64(optimal) * 1_000_000 * (1 - float64(conc-optimal)/40)
			}

			st := newRampState(4)
			settled, highest := 0, 0
			for i := 0; i < 400; i++ {
				if st.current > highest {
					highest = st.current
				}
				act := st.observe(cfg, curve(st.current), 0)
				if act.settle {
					settled = act.level
					break
				}
			}
			if settled == 0 {
				t.Fatal("ramp never settled")
			}
			if diff := settled - optimal; diff < -1 || diff > 1 {
				t.Fatalf("settled at %d, want within 1 of the true optimum %d (highest tried %d)", settled, optimal, highest)
			}
		})
	}
}

func TestLimiterEnforcesLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLimiter(ctx, 2)
	if !l.Acquire() || !l.Acquire() {
		t.Fatal("first two acquires should succeed")
	}

	// 第三个必须先阻塞，直到有人释放。
	blocked := make(chan bool, 1)
	go func() { blocked <- l.Acquire() }()

	select {
	case <-blocked:
		t.Fatal("third acquire must block while the limit is 2")
	case <-time.After(150 * time.Millisecond):
	}

	l.Release()
	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("acquire should succeed after a release")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not unblock after a release")
	}
}

// 收敛要把并发缩回实测最优点，闸门必须支持缩小。
func TestLimiterCanShrink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLimiter(ctx, 8)
	if !l.Acquire() {
		t.Fatal("acquire failed")
	}

	l.SetLimit(1)

	blocked := make(chan bool, 1)
	go func() { blocked <- l.Acquire() }()
	select {
	case <-blocked:
		t.Fatal("acquire must block after the limit shrank to 1")
	case <-time.After(150 * time.Millisecond):
	}

	l.Release()
	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("acquire should succeed after release")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not unblock")
	}
}

// Ctrl+C 之后阻塞在闸门上的 worker 必须能退出，不能把进程拖住。
func TestLimiterUnblocksOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := newLimiter(ctx, 1)
	if !l.Acquire() {
		t.Fatal("acquire failed")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	got := make(chan bool, 1)
	go func() {
		defer wg.Done()
		got <- l.Acquire()
	}()

	cancel()

	select {
	case ok := <-got:
		if ok {
			t.Fatal("acquire must fail after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not unblock on cancellation")
	}
	wg.Wait()
}

func TestNilLimiterIsUnrestricted(t *testing.T) {
	var l *limiter
	if !l.Acquire() {
		t.Fatal("nil limiter must always allow")
	}
	l.Release()   // 不能 panic
	l.SetLimit(4) // 不能 panic
}

func TestModeSnapshotDefaultsToFixed(t *testing.T) {
	autoMu.Lock()
	autoOn = false
	fixedLevel = 0
	autoMu.Unlock()

	setFixedConcurrency(12)
	view := modeSnapshot()
	if view.isAuto {
		t.Fatal("expected fixed mode")
	}
	if view.level != 12 || view.phase != "fixed" {
		t.Fatalf("unexpected snapshot: level=%d phase=%q", view.level, view.phase)
	}
	if view.best != 0 || len(view.steps) != 0 {
		t.Fatalf("fixed mode should have no peak or steps: best=%v steps=%d", view.best, len(view.steps))
	}
}

func TestModeSnapshotAuto(t *testing.T) {
	autoMu.Lock()
	autoOn = false
	fixedLevel = 0
	autoSteps = nil
	autoNote = ""
	autoMu.Unlock()

	setAutoState(8, "ramping")
	recordStep(rampSample{conc: 4, rate: 1000})
	setAutoPeak(2000, 8)

	view := modeSnapshot()
	if !view.isAuto {
		t.Fatal("expected auto mode")
	}
	if view.level != 8 || view.phase != "ramping" {
		t.Fatalf("unexpected snapshot: level=%d phase=%q", view.level, view.phase)
	}
	if view.best != 2000 || view.peakAt != 8 {
		t.Fatalf("unexpected peak: %v @ %d", view.best, view.peakAt)
	}
	if len(view.steps) != 1 || view.steps[0].conc != 4 {
		t.Fatalf("unexpected steps: %+v", view.steps)
	}

	// 快照必须是拷贝，改动切片不能影响内部状态。
	view.steps[0].conc = 999
	if again := modeSnapshot(); again.steps[0].conc != 4 {
		t.Fatal("modeSnapshot must return a copy of the steps")
	}
}

// 按键换算：10%/20%/30% 按比例，一倍是翻倍/减半。
func TestScaledConcurrency(t *testing.T) {
	cases := []struct {
		current int
		factor  float64
		want    int
	}{
		{16, 1.10, 18}, // 17.6 -> 18
		{16, 1.20, 19}, // 19.2 -> 19
		{16, 1.30, 21}, // 20.8 -> 21
		{16, 2.00, 32}, // 翻倍
		{16, 0.90, 14}, // 14.4 -> 14
		{16, 0.80, 13}, // 12.8 -> 13
		{16, 0.70, 11}, // 11.2 -> 11
		{16, 0.50, 8},  // 减半
		{1, 0.50, 1},   // 不能降到 0
		{1, 0.90, 1},   // 已经最小
		{2, 0.90, 1},   // 18 以下的整数运算也要至少变 1
		{300, 2.00, autoMaxConcurrency},
	}
	for _, c := range cases {
		if got := scaledConcurrency(c.current, c.factor); got != c.want {
			t.Errorf("scaledConcurrency(%d, %.2f) = %d, want %d", c.current, c.factor, got, c.want)
		}
	}
}

func TestScaleFactorKeys(t *testing.T) {
	// 1-4 扩容，5-8 缩容，其余按键一律不处理。
	for _, k := range []byte{'1', '2', '3', '4'} {
		f, _, ok := scaleFactor(k)
		if !ok || f <= 1 {
			t.Errorf("key %q should increase concurrency, got factor=%.2f ok=%v", k, f, ok)
		}
	}
	for _, k := range []byte{'5', '6', '7', '8'} {
		f, _, ok := scaleFactor(k)
		if !ok || f >= 1 {
			t.Errorf("key %q should decrease concurrency, got factor=%.2f ok=%v", k, f, ok)
		}
	}
	for _, k := range []byte{'0', '9', 'q', 'a', ' '} {
		if _, _, ok := scaleFactor(k); ok {
			t.Errorf("key %q should not be handled", k)
		}
	}
}

func TestConcurrencyLegendMentionsEveryKey(t *testing.T) {
	legend := concurrencyLegend()
	for _, k := range concurrencyKeys {
		if !strings.Contains(legend, string(k.key)+":") {
			t.Errorf("legend %q is missing key %q", legend, k.key)
		}
		if !strings.Contains(legend, k.label) {
			t.Errorf("legend %q is missing label %q", legend, k.label)
		}
	}
}

// 手动按键必须真的改变有效并发，并把模式切到 manual。
func TestAutoRampAdjustSwitchesToManual(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := &benchOptions{timeout: time.Second}
	var wg sync.WaitGroup

	ramp := newAutoRamp(ctx, opts, &wg, 8)
	if got := ramp.gate.Limit(); got != 8 {
		t.Fatalf("initial limit = %d, want 8", got)
	}
	// 不启动 worker，只验证闸门与状态流转。
	ramp.gate.SetLimit(8)

	ramp.Adjust(2.00, "x2")

	if got := ramp.gate.Limit(); got != 16 {
		t.Fatalf("after x2 the limit = %d, want 16", got)
	}
	if !ramp.isManual() {
		t.Fatal("a key press must switch the ramp to manual")
	}
	view := modeSnapshot()
	if view.phase != "manual" {
		t.Fatalf("phase = %q, want manual", view.phase)
	}
	if !strings.Contains(view.note, "x2") {
		t.Fatalf("note = %q, want it to mention the key label", view.note)
	}
}

// 取消之后不能再接受按键，否则会在 wg 上做危险的 Add。
func TestAutoRampAdjustIgnoresCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ramp := newAutoRamp(ctx, &benchOptions{}, &wg, 8)

	cancel()
	time.Sleep(10 * time.Millisecond)

	ramp.Adjust(2.00, "x2")
	if got := ramp.gate.Limit(); got != 8 {
		t.Fatalf("limit changed after cancellation: %d", got)
	}
}

// 小内存机器上，并发一高就会把机器推进 swap。护栏的阈值规则要钉死：
// 5% 与 64MB 取大者，宁可保守也不能把 512MB 的小机器拖进换页。
func TestMemoryFloor(t *testing.T) {
	const mb = 1 << 20
	cases := []struct {
		name      string
		total     uint64
		available uint64
		want      bool
	}{
		{"512MB box with 100MB free", 512 * mb, 100 * mb, false},
		{"512MB box with 40MB free", 512 * mb, 40 * mb, true},
		{"16GB box with 900MB free", 16 * 1024 * mb, 900 * mb, false},
		{"16GB box with 700MB free", 16 * 1024 * mb, 700 * mb, true},
		{"exactly at the floor", 512 * mb, 64 * mb, false},
		{"just under the floor", 512 * mb, 64*mb - 1, true},
	}
	for _, c := range cases {
		if got := memoryFloor(c.total, c.available); got != c.want {
			t.Errorf("%s: memoryFloor(%d,%d) = %v, want %v", c.name, c.total, c.available, got, c.want)
		}
	}
}
