package main

import (
	"context"
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
	isAuto, level, phase, best, _, steps := modeSnapshot()
	if isAuto {
		t.Fatal("expected fixed mode")
	}
	if level != 12 || phase != "fixed" {
		t.Fatalf("unexpected snapshot: level=%d phase=%q", level, phase)
	}
	if best != 0 || len(steps) != 0 {
		t.Fatalf("fixed mode should have no peak or steps: best=%v steps=%d", best, len(steps))
	}
}

func TestModeSnapshotAuto(t *testing.T) {
	autoMu.Lock()
	autoOn = false
	fixedLevel = 0
	autoSteps = nil
	autoMu.Unlock()

	setAutoState(8, "ramping")
	recordStep(rampStep{conc: 4, rate: 1000})
	setAutoPeak(2000, 8)

	isAuto, level, phase, best, bestConc, steps := modeSnapshot()
	if !isAuto {
		t.Fatal("expected auto mode")
	}
	if level != 8 || phase != "ramping" {
		t.Fatalf("unexpected snapshot: level=%d phase=%q", level, phase)
	}
	if best != 2000 || bestConc != 8 {
		t.Fatalf("unexpected peak: %v @ %d", best, bestConc)
	}
	if len(steps) != 1 || steps[0].conc != 4 {
		t.Fatalf("unexpected steps: %+v", steps)
	}

	// 快照必须是拷贝，改动切片不能影响内部状态。
	steps[0].conc = 999
	if _, _, _, _, _, again := modeSnapshot(); again[0].conc != 4 {
		t.Fatal("modeSnapshot must return a copy of the steps")
	}
}
