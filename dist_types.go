package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 分布式压测的主从两端共用这一份线协议定义。
// 主控把 TaskSpec 原样下发给从节点，从节点只按 PerNodeConc 决定本地开多少并发，
// 于是"一条任务长什么样"只有一处事实来源，不需要两端各自拼参数。
const (
	distProtoVersion = 1

	// 单节点并发上限与本地自动模式保持一致：256 条连接足以打满千兆，
	// 再往上只会把一台机器打到登不进去，反而失去观测能力。
	distMaxNodeConcurrency = 256
	distMaxNodesPerTask    = 64
	distMaxHeadersPerTask  = 32
	distMaxHeaderLen       = 512
	distMaxPostContent     = 64 << 10
	distMaxRandomSize      = 1 << 20
	distMaxTargetLen       = 2048
	distMaxNodeNameLen     = 32
	distMinDurationSec     = 1
	distMaxDurationSec     = 24 * 60 * 60
)

// 任务状态。主控用它决定控制台怎么显示、是否还要继续等结果。
const (
	taskRunning  = "running"
	taskDone     = "done"
	taskStopped  = "stopped"
	taskDegraded = "degraded"
	// taskErrored 既是节点上报的"整条任务失败"，也是主控侧 shard 的错误态，
	// 两者语义一致：这条分片没能产出可用数据。
	taskErrored = "error"
)

// 单个节点在某条任务里的状态。
const (
	shardAssigned = "assigned"
	shardRunning  = "running"
	shardDone     = "done"
	shardStopped  = "stopped"
	shardLost     = "lost"
	shardErrored  = "error"
)

// HeaderPair 是一条自定义请求头的线格式。
// 键和值都支持 Random<N> 占位符，展开逻辑沿用本地模式的 applyCustomHeaders。
type HeaderPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TaskSpec 是一条分布式压测任务的全部参数。
// 主控负责填 PerNodeConc、ID 与 CreatedAt，从节点只读不写。
type TaskSpec struct {
	ID          string       `json:"id"`
	Target      string       `json:"target"`
	Proto       string       `json:"proto"`
	DurationSec int          `json:"durationSec"`
	PerNodeConc int          `json:"perNodeConc"`
	TotalConc   int          `json:"totalConc"`
	NoReuse     bool         `json:"noReuse"`
	ProxySpoof  bool         `json:"proxySpoof"`
	XForwardFor bool         `json:"xforwardFor"`
	RandomPost  bool         `json:"randomPost"`
	RandomSize  int          `json:"randomSize"`
	PostContent string       `json:"postContent,omitempty"`
	Referer     string       `json:"referer,omitempty"`
	Headers     []HeaderPair `json:"headers,omitempty"`
	Family      string       `json:"family"`
	TimeoutSec  int          `json:"timeoutSec"`
	Note        string       `json:"note,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
}

// Progress 是从节点每秒上报一次的运行数据。
// 计数是"本任务开始以来的累计值"，主控据此算差分速率，
// 这样即使主从时钟有偏差，也不会算出一个荒谬的瞬时速率。
type Progress struct {
	TaskID    string `json:"taskId"`
	Seq       uint64 `json:"seq"`
	Requests  uint64 `json:"requests"`
	Failed    uint64 `json:"failed"`
	Bytes     uint64 `json:"bytes"`
	ElapsedMs int64  `json:"elapsedMs"`
	Conc      int    `json:"conc"`
	LastError string `json:"lastError,omitempty"`
}

// TaskResult 是节点跑完（或被打断）后交出的最终账本。
type TaskResult struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Requests  uint64 `json:"requests"`
	Failed    uint64 `json:"failed"`
	Bytes     uint64 `json:"bytes"`
	ElapsedMs int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`
}

// pollRequest 与 enroll/renew 那批结构已经随 WebSocket 通道一起删除：
// 现在入网、心跳、进度、结果、换证都走同一条加密连接（见 dist_secure.go）。

// taskCreateRequest 是控制台/API 建任务时提交的内容。
// 它内嵌 TaskSpec，因此客户端可以把任务参数平铺在同一个 JSON 里。
type taskCreateRequest struct {
	TaskSpec
	NodeIDs   []string `json:"nodeIds"`
	TotalConc int      `json:"totalConc"`
}

// normalize 只补结构性缺省值，不做"用户没写就替他打开某个开关"的猜测：
// 凡是影响测量口径的字段（超时、伪造头、随机体）都必须由调用方显式给出。
func (t *TaskSpec) normalize() {
	if t.ID == "" {
		t.ID = "task-" + randomHex(5)
	}
	if t.Proto == "" {
		t.Proto = "http1"
	}
	if t.Family == "" {
		t.Family = "ipv4"
	}
	// 分布式任务必须有明确的截止时间：一个"永远跑下去"的任务
	// 会在主控重启后变成无人认领的孤儿，且没有停止入口。
	if t.DurationSec == 0 {
		t.DurationSec = 60
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
}

// validate 把写错的参数挡在派发之前。校验失败必须是清晰的错误，
// 而不是让几十台机器各自用一个奇怪的值跑起来。
func (t *TaskSpec) validate() error {
	target := strings.TrimSpace(t.Target)
	if target == "" {
		return errors.New("任务缺少目标 URL")
	}
	if len(target) > distMaxTargetLen {
		return fmt.Errorf("目标 URL 超过 %d 字节", distMaxTargetLen)
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("目标 URL 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("目标 URL 必须以 http:// 或 https:// 开头，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("目标 URL 没有主机名")
	}
	if _, err := parseProto(t.Proto); err != nil {
		return err
	}
	switch strings.ToLower(t.Family) {
	case "ipv4", "ipv6", "any":
	default:
		return fmt.Errorf("family 只能是 ipv4、ipv6 或 any，收到 %q", t.Family)
	}
	if t.DurationSec < distMinDurationSec || t.DurationSec > distMaxDurationSec {
		return fmt.Errorf("时长必须在 %d..%d 秒之间，收到 %d", distMinDurationSec, distMaxDurationSec, t.DurationSec)
	}
	if t.PerNodeConc < 1 || t.PerNodeConc > distMaxNodeConcurrency {
		return fmt.Errorf("每节点并发必须在 1..%d 之间，收到 %d", distMaxNodeConcurrency, t.PerNodeConc)
	}
	if t.TimeoutSec < 0 || t.TimeoutSec > 3600 {
		return fmt.Errorf("每请求超时必须在 0..3600 秒之间，收到 %d", t.TimeoutSec)
	}
	if t.RandomSize < 0 || t.RandomSize > distMaxRandomSize {
		return fmt.Errorf("随机请求体大小必须在 0..%d 之间，收到 %d", distMaxRandomSize, t.RandomSize)
	}
	if len(t.PostContent) > distMaxPostContent {
		return fmt.Errorf("请求体超过 %d 字节", distMaxPostContent)
	}
	if len(t.Headers) > distMaxHeadersPerTask {
		return fmt.Errorf("请求头最多 %d 条", distMaxHeadersPerTask)
	}
	for _, h := range t.Headers {
		if !validHeaderName(h.Name) {
			return fmt.Errorf("请求头名称 %q 不合法", h.Name)
		}
		if len(h.Value) > distMaxHeaderLen {
			return fmt.Errorf("请求头 %s 的值超过 %d 字节", h.Name, distMaxHeaderLen)
		}
	}
	return nil
}

// validHeaderName 校验 RFC 7230 的 tchar 集合。
// 这条校验的意义不是"帮用户改错"，而是不让一个带 CRLF 的头名
// 把从节点的请求拼装变成头注入。
func validHeaderName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// splitPerNode 把总并发均摊到各节点，余数由前几个节点各多拿 1 条。
// 结果被夹在 1..distMaxNodeConcurrency：节点数多于总并发时每台至少 1 条，
// 总并发大到离谱时也不让单节点失控。
func splitPerNode(total, nodes int) int {
	if nodes < 1 {
		nodes = 1
	}
	if total < 1 {
		total = 1
	}
	per := total / nodes
	if total%nodes != 0 {
		per++
	}
	if per < 1 {
		per = 1
	}
	if per > distMaxNodeConcurrency {
		per = distMaxNodeConcurrency
	}
	return per
}

// sanitizeNodeName 把节点上报的名字收敛成一行短文本。
// 节点名会出现在控制台与审计日志里，控制字符、换行和过长内容
// 既会破坏排版，也让日志难以按行解析。
func sanitizeNodeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > distMaxNodeNameLen {
		runes = runes[:distMaxNodeNameLen]
	}
	return string(runes)
}

// randomHex 返回 n 字节的加密随机十六进制串。
// 随机源不可用时直接 panic：节点 ID、令牌 nonce 这类值一旦退化成可预测，
// 安全性就没了，宁可让运维看到环境坏了。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// shardTerminal 判断某个节点是否已经有结论，用于"全部结束就收尾"。
func shardTerminal(state string) bool {
	switch state {
	case shardDone, shardStopped, shardLost, shardErrored:
		return true
	}
	return false
}
