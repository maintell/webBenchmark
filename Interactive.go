package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// 单键调整并发的映射。1-4 提升，5-8 降低，两组一一对应，不需要任何修饰键。
var concurrencyKeys = []struct {
	key    byte
	label  string
	factor float64
}{
	{'1', "+10%", 1.10},
	{'2', "+20%", 1.20},
	{'3', "+30%", 1.30},
	{'4', "x2", 2.00},
	{'5', "-10%", 0.90},
	{'6', "-20%", 0.80},
	{'7', "-30%", 0.70},
	{'8', "/2", 0.50},
}

// concurrencyLegend 返回显示在状态栏的按键说明。
func concurrencyLegend() string {
	var b strings.Builder
	for i, k := range concurrencyKeys {
		switch {
		case i == 0:
		case i == len(concurrencyKeys)/2:
			b.WriteString("  |  ")
		default:
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%c:%s", k.key, k.label)
	}
	return b.String()
}

func scaleFactor(key byte) (factor float64, label string, ok bool) {
	for _, k := range concurrencyKeys {
		if k.key == key {
			return k.factor, k.label, true
		}
	}
	return 0, "", false
}

// scaledConcurrency 按比例换算出新的并发数。
// 结果是整数，且保证至少变化 1，否则在低并发下按 10% 会完全没有反应。
func scaledConcurrency(current int, factor float64) int {
	next := int(math.Round(float64(current) * factor))
	if next == current {
		if factor > 1 {
			next = current + 1
		} else if factor < 1 && current > 1 {
			next = current - 1
		}
	}
	if next < 1 {
		next = 1
	}
	if next > autoMaxConcurrency {
		next = autoMaxConcurrency
	}
	return next
}

// startKeyReader 把终端切到 raw 模式读取单键。
//
// raw 模式下 ISIG 被关闭，Ctrl+C 不再产生信号而是变成 0x03 字节，
// 所以这里要自己把它转成取消，否则用户会以为程序卡死了。
// 返回的 restore 必须保证在退出前被调用，否则终端会停在无回显状态。
func startKeyReader(ctx context.Context, cancel context.CancelFunc, onScale func(factor float64, label string)) (restore func(), ok bool) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, false
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, false
	}

	var once sync.Once
	restore = func() {
		once.Do(func() { _ = term.Restore(fd, oldState) })
	}

	go func() {
		buf := make([]byte, 8)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for _, b := range buf[:n] {
				if b == 3 { // Ctrl+C
					cancel()
					return
				}
				if factor, label, hit := scaleFactor(b); hit {
					onScale(factor, label)
				}
			}
		}
	}()

	// ctx 结束时恢复终端，避免 Ctrl+C 之后回显丢失。
	go func() {
		<-ctx.Done()
		restore()
	}()

	return restore, true
}
