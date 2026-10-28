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
func TestDecideRampFirstWindowGrows(t *testing.T) {
	next, flat, settle := decideRamp(4, 256, 0, 1_000_000, 0, 0, autoMinGain, autoMaxFlatWindows)
	if settle || flat != 0 {
		t.Fatalf("first window should not settle: next=%d flat=%d settle=%v", next, flat, settle)
	}
	if next != 8 {
		t.Fatalf("first window should double concurrency, got %d", next)
	}
}

func TestDecideRampGrowsWhenBeatingPeak(t *testing.T) {
	// 比峰值高 50%，远超 10% 门槛，应该继续加倍。
	next, flat, settle := decideRamp(8, 256, 2_000_000, 3_000_000, 0, 0, autoMinGain, autoMaxFlatWindows)
	if settle || flat != 0 {
		t.Fatalf("a 50%% gain over the peak should not settle: next=%d flat=%d settle=%v", next, flat, settle)
	}
	if next != 16 {
		t.Fatalf("expected concurrency 16, got %d", next)
	}
}

// 判定基准必须是历史峰值，不是上一个窗口：
// 实测跨境链路相邻窗口能抖 ±30%，拿相邻窗口比会把抖动当成趋势。
func TestDecideRampComparesAgainstPeakNotPreviousWindow(t *testing.T) {
	const peak = 3_000_000
	// 上一个窗口只有 2.0MB/s，本窗口回到 3.05MB/s —— 比上一个窗口涨了 52%，
	// 但只比历史峰值高 1.7%，不应该再加并发。
	next, flat, settle := decideRamp(16, 256, peak, 3_050_000, 0, 0, autoMinGain, autoMaxFlatWindows)
	if settle {
		t.Fatal("first window below the threshold must not settle yet")
	}
	if next != 16 {
		t.Fatalf("must not grow when the peak is not beaten, got %d", next)
	}
	if flat != 1 {
		t.Fatalf("flat counter = %d, want 1", flat)
	}
}

// 单次没有提升不能立刻收敛，要原地再测一个窗口。
func TestDecideRampHoldsThenSettles(t *testing.T) {
	next, flat, settle := decideRamp(16, 256, 3_000_000, 3_050_000, 0, 0, autoMinGain, autoMaxFlatWindows)
	if settle || next != 16 || flat != 1 {
		t.Fatalf("unexpected first flat window: next=%d flat=%d settle=%v", next, flat, settle)
	}

	_, flat2, settle2 := decideRamp(16, 256, 3_050_000, 3_060_000, flat, 0, autoMinGain, autoMaxFlatWindows)
	if !settle2 {
		t.Fatal("second consecutive flat window must settle")
	}
	if flat2 != autoMaxFlatWindows {
		t.Fatalf("flat counter = %d, want %d", flat2, autoMaxFlatWindows)
	}
}

func TestDecideRampSettlesAtMaxConcurrency(t *testing.T) {
	_, _, settle := decideRamp(autoMaxConcurrency, autoMaxConcurrency, 1, 2, 0, 0, autoMinGain, autoMaxFlatWindows)
	if !settle {
		t.Fatal("must settle once the maximum concurrency is reached")
	}
}

func TestDecideRampCapsAtMaxConcurrency(t *testing.T) {
	next, _, settle := decideRamp(200, autoMaxConcurrency, 1_000_000, 5_000_000, 0, 0, autoMinGain, autoMaxFlatWindows)
	if settle {
		t.Fatal("should still grow towards the cap")
	}
	if next != autoMaxConcurrency {
		t.Fatalf("concurrency must be capped at %d, got %d", autoMaxConcurrency, next)
	}
}

// 窗口里有失败说明目标已经在拒绝负载，此时加并发只会更糟。
func TestDecideRampDoesNotGrowOnFailures(t *testing.T) {
	next, flat, settle := decideRamp(8, 256, 1_000_000, 3_000_000, 0, 3, autoMinGain, autoMaxFlatWindows)
	if settle {
		t.Fatal("a single window with failures should not settle immediately")
	}
	if next != 8 {
		t.Fatalf("must not raise concurrency while requests fail, got %d", next)
	}
	if flat != 1 {
		t.Fatalf("flat counter = %d, want 1", flat)
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
	recordStep(rampStep{conc: 4, rate: 1000})
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
