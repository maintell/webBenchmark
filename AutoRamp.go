package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/mem"
)

const (
	// 自动模式从一个保守的并发起步，然后逐级提升，直到吞吐不再提升。
	autoStartConcurrency = 4
	// 上限只是防止失控，正常链路远到不了。
	autoMaxConcurrency = 256
	// 单个测量窗口。太短会被握手和 TCP 慢启动污染，太长则整轮等太久。
	autoWindow = 4 * time.Second
	// 爬坡阶段的门槛：比历史峰值高出这个比例才算"还有提升"，用它抵抗链路抖动。
	autoMinGain = 0.10
	// 连续多少个"没提升"的窗口才结束爬坡，避免被单次抖动骗过去。
	autoMaxFlatWindows = 2
	// 爬坡步进因子。以前是硬编码翻倍（4->8->16->32），分辨率太粗：
	// 真峰在 22 时只能落在 16，白丢一到两成吞吐；落在 32 又直接掉进自残区。
	autoStep = 1.5
	// 精细收敛阶段：门槛比爬坡小一个量级，但每个候选档要取多个窗口的均值，
	// 否则单窗口的 ±10% 抖动足以让二分走错方向。
	autoRefineGain    = 0.03
	autoRefineWindows = 2
	// 精细收敛最多试几个候选档，防止在平坦段无限磨。
	autoRefineMaxTries = 8
	// 区间窄到这个比例就停手（low*5% 与 2 取大者）。
	// 曾经取 10%：真峰在 37 时会停在 39，白丢 5% 吞吐。
	autoRefineTol   = 0.05
	autoRefineFloor = 2
	// 状态栏最多回显多少条爬坡记录。
	autoVisibleSteps = 5
	// 抖动估计的上限：门槛再高也不该高过这个值，否则永远找不到提升。
	autoNoiseCeiling = 0.35
	// 自动模式的可用内存下限：低于它就停止加压。
	minFreeMemory = 64 << 20
)

// memoryFloor 判断可用内存是否已经低到不该再加并发。
//
// 内核会给每条连接按 BDP 自动开接收缓冲（实测高 RTT 链路上每连接约 2.5MB），
// 小内存机器上并发一高就把机器推进 swap；swap 再变成磁盘 IO，吞吐反而塌下来。
// 所以这里不是"优化"而是护栏：宁可少测一点，也不要把机器拖进换页。
func memoryFloor(total, available uint64) bool {
	floor := total / 20 // 5%
	if floor < minFreeMemory {
		floor = minFreeMemory
	}
	return available < floor
}

// memoryLow 取一次当前可用内存并给出判断；取不到时按"不低"处理，宁可不干预。
func memoryLow() bool {
	vm, err := mem.VirtualMemory()
	if err != nil || vm == nil {
		return false
	}
	return memoryFloor(vm.Total, vm.Available)
}

// rampSample 记录一次测量窗口的结果，用于回显。
type rampSample struct {
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
	autoLow      int
	autoHigh     int
	autoSteps    []rampSample
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

// setAutoBracket 回显"已确认的下界 / 已否定的上界"，收敛过程中用户能看出还差多少。
func setAutoBracket(low, high int) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoLow = low
	autoHigh = high
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

func recordStep(sample rampSample) {
	autoMu.Lock()
	defer autoMu.Unlock()
	autoSteps = append(autoSteps, sample)
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
	low    int
	high   int
	steps  []rampSample
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
		low:    autoLow,
		high:   autoHigh,
		steps:  append([]rampSample(nil), autoSteps...),
		canKey: interactive,
	}
}

// ---------------------------------------------------------------------------
// 收敛算法
//
// 分两段：先用几何步进爬到"再好也涨不动"的位置，再在最后成功的一档与第一档
// 失败的并发之间做几何二分，逐档取均值比较。爬坡负责快，二分负责准。
//
// 整个判定写成不启动 goroutine 的纯状态机，测试可以直接驱动它。
// ---------------------------------------------------------------------------

type rampPhase int

const (
	rampClimb rampPhase = iota
	rampRefine
	rampDuel
	rampSettled
)

func (p rampPhase) String() string {
	switch p {
	case rampRefine:
		return "refine"
	case rampDuel:
		return "duel"
	case rampSettled:
		return "settled"
	default:
		return "climb"
	}
}

// rampConfig 是全部可调参数。零值不可用，一律通过 benchOptions.rampConfig() 构造。
type rampConfig struct {
	step          float64       // 爬坡乘数，>1
	minGain       float64       // 爬坡门槛
	refineGain    float64       // 收敛门槛
	refineWindows int           // 每个候选档测几个窗口
	refineTries   int           // 最多试几个候选档
	refineTol     float64       // 区间宽度容差（相对下界）
	maxFlat       int           // 连续几个窗口没提升就转入收敛
	maxConc       int           // 并发上限
	window        time.Duration // 单个测量窗口
	duel          bool          // 收敛前是否做一次相邻档对决
}

// rampConfig 把命令行参数和默认值合起来。
// 零值字段一律回退到默认值，测试里可以直接给空 benchOptions。
func (o *benchOptions) rampConfig() rampConfig {
	cfg := rampConfig{
		step:          autoStep,
		minGain:       autoMinGain,
		refineGain:    autoRefineGain,
		refineWindows: autoRefineWindows,
		refineTries:   autoRefineMaxTries,
		refineTol:     autoRefineTol,
		maxFlat:       autoMaxFlatWindows,
		maxConc:       autoMaxConcurrency,
		window:        autoWindow,
		duel:          true,
	}
	if o == nil {
		return cfg
	}
	cfg.duel = !o.rampNoDuel
	if o.rampStep > 1 {
		cfg.step = o.rampStep
	}
	if o.rampMinGain > 0 {
		cfg.minGain = o.rampMinGain
	}
	if o.rampRefineGain > 0 {
		cfg.refineGain = o.rampRefineGain
	}
	if o.rampWindow > 0 {
		cfg.window = o.rampWindow
	}
	return cfg
}

// rampState 只保存判定所需的状态，方便测试逐窗口喂数据。
type rampState struct {
	phase    rampPhase
	current  int
	best     float64
	bestConc int
	flat     int
	low      int // 已验证"还行"的最大并发
	high     int // 已验证"不行"的最小并发，0 表示还没探到
	noise    float64
	breakout bool // 这一轮平台是否已经做过突破探测

	prevRate float64
	prevConc int

	tries   int
	cand    int
	candSum float64
	candN   int

	// 收敛前的相邻档对决：先用两个窗口复核现役，再给上一档同样两个窗口。
	duelDone  bool
	duelN     int
	duelSum   float64
	duelInc   float64
	duelChall int
	challN    int
	challSum  float64
}

// rampAction 是状态机对外的唯一输出：下一档并发、当前阶段、是否收敛。
type rampAction struct {
	level  int
	phase  rampPhase
	settle bool
}

func newRampState(start int) *rampState {
	return &rampState{phase: rampClimb, current: start, low: start}
}

// grow 按乘数把并发抬到下一档，至少 +1，避免在低并发上取整后原地踏步。
func grow(current int, step float64, maxConc int) int {
	if step <= 1 {
		step = autoStep
	}
	next := int(math.Ceil(float64(current) * step))
	if next <= current {
		next = current + 1
	}
	if next > maxConc {
		next = maxConc
	}
	return next
}

// gate 是爬坡门槛：命令行给的死区与实测抖动取大者。
//
// 跨境链路的单窗口抖动就能到 ±20%：固定 10% 死区在这种链路上要么把抖动当提升
// 继续加并发，要么反过来把真实提升当抖动。这里用同一档相邻两个窗口的差值估抖动，
// 抖动大就把门槛抬起来，链路稳就回落到命令行给的死区。
func (s *rampState) gate(cfg rampConfig) float64 {
	g := cfg.minGain
	if s.noise > g {
		g = s.noise
	}
	if g > autoNoiseCeiling {
		g = autoNoiseCeiling
	}
	return g
}

// observeNoise 用同一档相邻两个窗口的相对差估计抖动，做一次指数平滑。
func (s *rampState) observeNoise(rate float64) {
	if s.prevConc != s.current || s.prevRate <= 0 || rate <= 0 {
		return
	}
	hi := math.Max(rate, s.prevRate)
	if hi <= 0 {
		return
	}
	diff := math.Abs(rate-s.prevRate) / hi
	if s.noise == 0 {
		s.noise = diff
		return
	}
	s.noise = 0.5*s.noise + 0.5*diff
}

// observe 吃进一个测量窗口，返回下一步动作。
func (s *rampState) observe(cfg rampConfig, rate float64, windowFailed uint64) rampAction {
	switch s.phase {
	case rampRefine:
		return s.refine(cfg, rate, windowFailed)
	case rampDuel:
		return s.duel(cfg, rate, windowFailed)
	case rampSettled:
		return rampAction{level: s.level(), phase: rampSettled, settle: true}
	default:
		return s.climb(cfg, rate, windowFailed)
	}
}

// duel 是收敛前的最后一道校验：现役档和上一档各测同样多的窗口，均值高者胜。
//
// 为什么需要：峰值原本取"单窗最高观测"，而单窗在跨境链路上能抖 ±20%。
// 实测一次是 9 并发拿到 61.4（单窗），14 并发两窗是 68.1/72.1，算法却停在 9。
// 用同样窗口数正面比一次，才能把"幸运窗口"和"真的更高"分开。
func (s *rampState) duel(cfg rampConfig, rate float64, windowFailed uint64) rampAction {
	windows := cfg.refineWindows
	if windows < 1 {
		windows = 1
	}

	// 前半段：复核现役档。
	if s.duelN < windows {
		if windowFailed == 0 {
			s.duelSum += rate
			s.duelN++
		}
		if s.duelN < windows {
			return rampAction{level: s.bestConc, phase: rampDuel}
		}
		s.duelInc = s.duelSum / float64(s.duelN)
		s.duelChall = grow(s.bestConc, cfg.step, cfg.maxConc)
		if s.duelChall <= s.bestConc || s.duelChall > cfg.maxConc {
			return s.finish(s.duelInc)
		}
		s.current = s.duelChall
		return rampAction{level: s.duelChall, phase: rampDuel}
	}

	// 后半段：挑战者拿同样多的窗口。
	if windowFailed == 0 {
		s.challSum += rate
		s.challN++
	}
	if s.challN < windows {
		return rampAction{level: s.duelChall, phase: rampDuel}
	}
	chall := s.challSum / float64(s.challN)
	if chall > s.duelInc*(1+cfg.refineGain) {
		s.best, s.bestConc, s.low = chall, s.duelChall, s.duelChall
		return s.finish(chall)
	}
	// 挑战者没赢：用复核后的均值做峰值，不保留那个幸运窗口的数字。
	return s.finish(s.duelInc)
}

// finish 真正收工：并发停在 bestConc，峰值取最后一次确认的均值。
func (s *rampState) finish(peak float64) rampAction {
	s.phase = rampSettled
	s.duelDone = true
	if peak > 0 {
		s.best = peak
	}
	return rampAction{level: s.level(), phase: rampSettled, settle: true}
}

func (s *rampState) climb(cfg rampConfig, rate float64, windowFailed uint64) rampAction {
	// 先拿上一次窗口（可能还在同一档）估抖动，再把本次记下来供下一次比较。
	s.observeNoise(rate)
	s.prevRate, s.prevConc = rate, s.current

	// 零吞吐既不算提升也不算抖动：连着几个窗口都没数据就该停，不能一路翻到上限。
	prevBest := s.best
	// 观测到的新高一律记下来：最终答案要落在真的跑出最高吞吐那一档，
	// 不能因为"只比峰值高 6%，还在死区里"就把更高的实测值扔掉。
	if rate > s.best {
		s.best, s.bestConc = rate, s.current
		s.low = s.current
	}
	// 但"继续加并发"必须跨过死区：抖动不该驱动爬坡。
	//
	// 突破探测例外：爬坡已经连续两窗确认过平台，这一档只要跑出任何新高，
	// 就说明那个"平台"是假的（实测 4 档 41.1、6 档 27.6 两窗、9 档 43.2），
	// 再用 10% 死区去卡就白白停在半路。
	threshold := s.gate(cfg)
	if s.breakout {
		threshold = 0
	}
	improved := rate > 0 && (prevBest == 0 || rate > prevBest*(1+threshold))

	if windowFailed == 0 && improved {
		s.flat = 0
		s.breakout = false
		next := grow(s.current, cfg.step, cfg.maxConc)
		if next <= s.current {
			return s.settle(cfg)
		}
		s.current = next
		return rampAction{level: next, phase: rampClimb}
	}

	s.flat++
	if s.flat < cfg.maxFlat {
		// 原地复测一档：链路上的单次抖动不该直接终结爬坡。
		return rampAction{level: s.current, phase: rampClimb}
	}

	// 平台出现：先往上再探一档，确认它不是一次低谷。
	//
	// 只靠"这一档没涨"就收工会被单点抖动骗掉：实测某次 6 并发 48.8MB/s、
	// 9 并发连续两个窗口都只有 48.5，于是判定平台落在 [6,9] 并收敛到 6；
	// 同一台机器固定 8 并发却能跑到 62.6MB/s——真峰被一次偶然的低谷挡在外面。
	// 突破探测给平台一次翻案机会：探到了就继续爬，探不到才是真的平台。
	if !s.breakout {
		s.breakout = true
		if probe := grow(s.current, cfg.step, cfg.maxConc); probe > s.current {
			s.current = probe
			return rampAction{level: probe, phase: rampClimb}
		}
	}

	// 探测点也没赚到：它就是这个区间真正的上界。
	// 只有在"确认更差"的位置才设 high——平缓段里某一档只是没跨过死区，
	// 但它可能仍是实测最高（峰值在它身上），那就不是上界。
	s.high = s.current
	return s.startRefine(cfg)
}

// startRefine 拿着 [low, high] 进入精细收敛；区间已经足够窄就直接收工。
func (s *rampState) startRefine(cfg rampConfig) rampAction {
	if s.bestConc <= 0 {
		s.bestConc = s.low
	}
	if s.bestConc <= 0 {
		s.bestConc = s.current
	}
	s.phase = rampRefine
	return s.nextCandidate(cfg)
}

// nextCandidate 在峰值的两侧交替取几何中点。
//
// 只从"峰值档往失败档"单向二分是不够的：真峰在 37、已测 32 和 48 时，
// 第一次取 39（比 32 快），区间随即变成 [39,48]，真正的峰被永久丢在区间外，
// 最后停在 39。所以这里保留 low < best < high 三点，哪一侧的缝更宽就往哪边探。
func (s *rampState) nextCandidate(cfg rampConfig) rampAction {
	if s.high <= 0 || s.bestConc <= 0 {
		return s.settle(cfg)
	}

	tol := int(math.Round(float64(s.bestConc) * cfg.refineTol))
	if tol < autoRefineFloor {
		tol = autoRefineFloor
	}
	left := s.bestConc - s.low
	right := s.high - s.bestConc
	leftOK := left > tol && left >= 2
	rightOK := right > tol && right >= 2
	if (!leftOK && !rightOK) || s.tries >= cfg.refineTries {
		return s.settle(cfg)
	}

	var cand int
	if rightOK && (!leftOK || right >= left) {
		cand = geoMid(s.bestConc, s.high)
	} else {
		cand = geoMid(s.low, s.bestConc)
	}
	if cand <= 0 {
		return s.settle(cfg)
	}

	s.tries++
	s.cand = cand
	s.candSum, s.candN = 0, 0
	s.current = cand
	return rampAction{level: cand, phase: rampRefine}
}

// geoMid 返回两个并发档之间的几何中点，保证严格落在开区间内。
func geoMid(a, b int) int {
	if b-a < 2 {
		return 0
	}
	mid := int(math.Round(math.Sqrt(float64(a) * float64(b))))
	if mid <= a {
		mid = a + 1
	}
	if mid >= b {
		mid = b - 1
	}
	return mid
}

func (s *rampState) refine(cfg rampConfig, rate float64, windowFailed uint64) rampAction {
	// 候选档直接报错：这一档不可用，没必要求均值。按测点在峰值的哪一侧收紧对应边界。
	if windowFailed > 0 {
		s.pushBound()
		return s.nextCandidate(cfg)
	}

	s.candSum += rate
	s.candN++
	if s.candN < cfg.refineWindows {
		return rampAction{level: s.cand, phase: rampRefine}
	}

	avg := s.candSum / float64(s.candN)
	if s.best <= 0 || avg > s.best*(1+cfg.refineGain) {
		// 只有真的高出收敛门槛才搬动最优点；差距在门槛内按"打平"处理，
		// 打平时保留更小的并发——同样的吞吐，少占连接、少吃 CPU 的那档更好。
		//
		// 搬动时把旧峰值档留作新的边界，而不是直接用它替换：
		// 单点比较无法排除"真峰藏在旧峰值与新测点之间"。
		if s.cand > s.bestConc {
			s.low = s.bestConc
		} else {
			s.high = s.bestConc
		}
		s.best, s.bestConc = avg, s.cand
	} else {
		s.pushBound()
	}
	return s.nextCandidate(cfg)
}

// pushBound 把当前候选档记为"不可取"的一侧边界。
func (s *rampState) pushBound() {
	if s.cand > s.bestConc {
		s.high = s.cand
		return
	}
	s.low = s.cand
}

func (s *rampState) level() int {
	if s.bestConc > 0 {
		return s.bestConc
	}
	if s.low > 0 {
		return s.low
	}
	return s.current
}

// settle 收敛到实测最优点，而不是最后试到的那个并发。
// 收工前先做一次相邻档对决（可用 -ramp-duel=false 关掉）。
func (s *rampState) settle(cfg rampConfig) rampAction {
	if !s.duelDone && cfg.duel && s.best > 0 && s.bestConc > 0 && s.bestConc < cfg.maxConc {
		s.phase = rampDuel
		s.current = s.bestConc
		s.duelN, s.duelSum = 0, 0
		s.challN, s.challSum = 0, 0
		return rampAction{level: s.bestConc, phase: rampDuel}
	}
	return s.finish(0)
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
	worker := runBenchmark
	if r.opts.raw {
		worker = runRawBenchmark
	}
	r.mu.Lock()
	for r.spawned < n {
		r.spawned++
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			worker(r.ctx, r.opts, getTargetURL)
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

// runAutoRamp 先用几何步进爬到平台，再在平台附近做二分收敛。
func runAutoRamp(ctx context.Context, opts *benchOptions, wg *sync.WaitGroup, start int) {
	cfg := opts.rampConfig()

	if start < 1 {
		start = autoStartConcurrency
	}
	if start > cfg.maxConc {
		start = cfg.maxConc
	}

	ramp := newAutoRamp(ctx, opts, wg, start)
	opts.gate = ramp.gate
	// 这里刻意不在退出时清空 activeRamp：
	// 用户手动接管后自动判定会停，但按键必须继续有效，
	// 否则第二次按键就落空了。ctx 结束后 Adjust 自己会忽略输入。
	activeRamp.Store(ramp)

	ramp.setConcurrency(start)
	setAutoState(start, rampClimb.String())
	setAutoBracket(start, 0)

	st := newRampState(start)

	// 第一个窗口只用来预热，不参与判定：握手、DNS、TCP 慢启动全在里面，
	// 实测 4 并发的第一窗只有稳定值的四成，拿它当基准会把起始档系统性看低。
	// 丢掉一窗只损失 window 秒，换来一个不带偏差的起点。
	if !waitOrDone(ctx, cfg.window) {
		return
	}
	prevBytes := stats.bytes.Load()
	prevFailed := stats.failed.Load()

	for {
		if !waitOrDone(ctx, cfg.window) {
			return
		}
		// 用户已经手动接管，自动判定就此停止，不再和用户抢控制权。
		if ramp.isManual() {
			return
		}

		curBytes := stats.bytes.Load()
		curFailed := stats.failed.Load()
		windowFailed := curFailed - prevFailed
		rate := float64(curBytes-prevBytes) / cfg.window.Seconds()

		measured := st.current
		step := rampSample{conc: measured, rate: rate, failed: windowFailed}
		if st.best > 0 {
			step.gain = (rate - st.best) / st.best
			step.hasGain = true
		}
		recordStep(step)

		act := st.observe(cfg, rate, windowFailed)
		prevBytes, prevFailed = curBytes, curFailed

		if act.settle {
			ramp.setConcurrency(act.level)
			setAutoState(act.level, rampSettled.String())
			setAutoBracket(st.bestConc, st.high)
			setAutoPeak(st.best, st.bestConc)
			fmt.Fprintf(os.Stderr, "auto: settled at %d connections, peak %s/s\n",
				act.level, readableBytes(st.best))
			return
		}

		// 内存护栏：再往上加并发就会把机器推进 swap（swap 又变成磁盘 IO），
		// 那不是在找上限，是在制造瓶颈。就地停在实测最优点并说明原因。
		if st.bestConc > 0 && act.level > st.bestConc && memoryLow() {
			ramp.setConcurrency(st.bestConc)
			setAutoState(st.bestConc, rampSettled.String())
			setAutoBracket(st.bestConc, st.high)
			setAutoPeak(st.best, st.bestConc)
			fmt.Fprintf(os.Stderr, "auto: settled at %d connections (free memory below the floor), peak %s/s\n",
				st.bestConc, readableBytes(st.best))
			return
		}

		if act.level != ramp.gate.Limit() {
			ramp.setConcurrency(act.level)
		}
		setAutoState(act.level, act.phase.String())
		setAutoBracket(st.bestConc, st.high)
	}
}
