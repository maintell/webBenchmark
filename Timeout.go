package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// errIdleTimeout 是"连续 t 秒没有任何数据"的哨兵错误。
// 不用 context.DeadlineExceeded 是因为事后必须能区分：
// 这一次是人按了 Ctrl+C，还是目标真的不吐数据。
var errIdleTimeout = errors.New("no data within the per-request timeout")

// idleGuard 把 -t 的语义从"整个请求的总时限"改成"空闲时限"。
//
// 旧实现是 client.Timeout：一个 76 MB 的文件在 1 MB/s 的链路上必然超过默认
// 30 秒，于是每个请求都被判失败——目标没有任何问题，是秒表掐得太死。这类假失败
// 会污染自动模式对目标压力的判断，让它在真正的最优点之前就收敛。
// 现在只有连续 t 秒没有任何字节进来才算超时，持续传输的请求永远不会被误杀。
type idleGuard struct {
	d      time.Duration
	cancel context.CancelCauseFunc
	timer  *time.Timer
	once   sync.Once
}

// newIdleGuard 返回带空闲看门的 ctx；d <= 0 表示关闭超时。
func newIdleGuard(parent context.Context, d time.Duration) (context.Context, *idleGuard) {
	ctx, cancel := context.WithCancelCause(parent)
	g := &idleGuard{d: d, cancel: cancel}
	if d > 0 {
		// 报错里带上时长和下一步，用户看到 "no data for 30s" 就知道该调 -t 还是查目标。
		reason := fmt.Errorf("%w: no data for %s (raise -t for slow targets)", errIdleTimeout, d)
		g.timer = time.AfterFunc(d, func() { cancel(reason) })
	}
	return ctx, g
}

// touch 每收到一批数据就重置秒表。
func (g *idleGuard) touch() {
	if g == nil || g.timer == nil {
		return
	}
	g.timer.Stop()
	g.timer.Reset(g.d)
}

// stop 释放定时器并取消请求 ctx。
//
// 必须在响应体读完之后调用：提前取消会让 Transport 直接拆掉连接，
// 每请求都重新握手，压测结果就变成测握手速度了。
func (g *idleGuard) stop() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.timer != nil {
			g.timer.Stop()
		}
		g.cancel(nil)
	})
}

// isTimeoutError 判断这次失败是"秒表到点"还是"目标拒绝/出错"。
func isTimeoutError(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	if ctx != nil && errors.Is(context.Cause(ctx), errIdleTimeout) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
