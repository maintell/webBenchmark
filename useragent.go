package main

import "github.com/EDDYCJY/fake-useragent"

const defaultUserAgent = "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

// userAgentPool 在启动时把随机 UA 固化成一小撮字符串。
//
// fake-useragent 内部持有一个没有加锁的 *rand.Rand，几十个压测 goroutine
// 同时调用 browser.Android() 就是一次实打实的数据竞争（-race 可稳定复现，
// Go 1.24 起 rand 源也不再容忍这种用法）。热路径上每个请求都去随机取一次
// 既没必要也不安全，启动时取够样本，之后只做一次下标选择。
var userAgentPool = buildUserAgentPool(64)

func buildUserAgentPool(n int) []string {
	seen := make(map[string]struct{}, n)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ua := browser.Android()
		if ua == "" {
			continue
		}
		if _, ok := seen[ua]; ok {
			continue
		}
		seen[ua] = struct{}{}
		out = append(out, ua)
	}
	if len(out) == 0 {
		out = append(out, defaultUserAgent)
	}
	return out
}

func randomUserAgent() string {
	return userAgentPool[randIntn(len(userAgentPool))]
}
