package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// statsBaseline 是任务开始时的计数器基线。
//
// 压测热路径上的原子计数器是包级全局的（改成每任务一套会给每个请求
// 多加一层间接寻址，而这条路径每秒要跑几万次）。从节点同一时刻只跑一条任务，
// 所以用"开始时的快照 + 差值"就能得到本任务的准确账本。
type statsBaseline struct {
	requests uint64
	failed   uint64
	bytes    uint64
}

func statsNow() statsBaseline {
	return statsBaseline{
		requests: stats.requests.Load(),
		failed:   stats.failed.Load(),
		bytes:    stats.bytes.Load(),
	}
}

func (b statsBaseline) snapshot(started time.Time, conc int) Progress {
	return Progress{
		Requests:  stats.requests.Load() - b.requests,
		Failed:    stats.failed.Load() - b.failed,
		Bytes:     stats.bytes.Load() - b.bytes,
		ElapsedMs: time.Since(started).Milliseconds(),
		Conc:      conc,
		LastError: shortText(lastErrorMessage(), 200),
	}
}

// benchOptions 把线上任务参数翻译成本地压测引擎认识的选项。
// 两套参数刻意一一对应：分布式与单机测的必须是同一件事，
// 否则"本机 200MB/s、集群 120MB/s"这种结论就没法解释。
func (t *TaskSpec) benchOptions() *benchOptions {
	mode, err := parseProto(t.Proto)
	if err != nil {
		mode = protoHTTP1
	}
	family := familyIPv4
	switch strings.ToLower(t.Family) {
	case "ipv6":
		family = familyIPv6
	case "any":
		family = familyAny
	}
	headers := make(headersList, 0, len(t.Headers))
	for _, h := range t.Headers {
		headers = append(headers, header{key: h.Name, value: h.Value})
	}
	return &benchOptions{
		postContent: t.PostContent,
		randomPost:  t.RandomPost,
		randomSize:  t.RandomSize,
		referer:     t.Referer,
		xforwardfor: t.XForwardFor,
		headers:     headers,
		noReuse:     t.NoReuse,
		timeout:     time.Duration(t.TimeoutSec) * time.Second,
		family:      family,
		proxySpoof:  t.ProxySpoof,
		proto:       mode,
	}
}

// clearLastError 清空"最后一条错误"。
// 全局错误槽是给单机交互模式用的，跨任务复用会把上一轮的错误
// 算到这一轮的结论里，所以每条任务开始前都清一次。
func clearLastError() {
	errMu.Lock()
	lastError = ""
	errMu.Unlock()
}

// runDistTask 在本机执行一条分布式任务，直到到点、被主控叫停或进程退出。
//
// stopCh 传的是"为什么停"：deadline 表示任务自己跑满了时长（正常完成），
// stopped 表示控制台按停。两者必须区分，否则一条跑满时长的任务会被
// 记成"已停止"，历史列表里全是误报。
func runDistTask(ctx context.Context, spec *TaskSpec, conc int, onSample func(Progress), stopCh <-chan string) TaskResult {
	if conc < 1 {
		conc = 1
	}
	if conc > distMaxNodeConcurrency {
		conc = distMaxNodeConcurrency
	}
	duration := time.Duration(spec.DurationSec) * time.Second
	if duration <= 0 {
		duration = time.Minute
	}

	opts := spec.benchOptions()
	target := spec.Target
	clearLastError()
	base := statsNow()
	started := time.Now()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runBenchmark(runCtx, opts, func() string { return target })
		}()
	}

	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	reason := "deadline"
loop:
	for {
		select {
		case <-deadline.C:
			reason = "deadline"
			break loop
		case r := <-stopCh:
			if r != "" {
				reason = r
			} else {
				reason = "stopped"
			}
			break loop
		case <-ctx.Done():
			reason = "stopped"
			break loop
		case <-ticker.C:
			if onSample != nil {
				onSample(base.snapshot(started, conc))
			}
		}
	}
	cancel()
	wg.Wait()

	// 收尾再补一帧：短任务常常只有一帧采样，
	// 主控拿不到第二个点就算不出速率，曲线的最后一秒也永远是空的。
	if onSample != nil {
		onSample(base.snapshot(started, conc))
	}

	res := TaskResult{
		TaskID:    spec.ID,
		Requests:  stats.requests.Load() - base.requests,
		Failed:    stats.failed.Load() - base.failed,
		Bytes:     stats.bytes.Load() - base.bytes,
		ElapsedMs: time.Since(started).Milliseconds(),
	}
	switch {
	case reason != "deadline":
		res.Status = taskStopped
	case res.Requests == 0:
		// 一个请求都没发出去，说明是配置或链路问题（DNS、证书、地址族），
		// 必须让主控看到 error，而不是一个漂亮的全零结果。
		res.Status = taskErrored
		res.Error = shortText(lastErrorMessage(), 300)
		if res.Error == "" {
			res.Error = "任务没有发出任何请求"
		}
	default:
		res.Status = taskDone
	}
	return res
}
