package main

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// GetHttpLocation 返回 rawURL 第一跳跳转的目标地址；没有跳转或探测失败时返回空串。
//
// 这里刻意不跟随重定向：
//   - 只看响应头，探测本身不会给真正的目标带来额外流量；
//   - 跳转目标不可达时（探测用的一次 GET 会失败）依然能拿到 Location，
//     旧实现跟随跳转后一旦失败就什么都拿不到。
//
// 旧实现还有个硬伤：直接读 resp.Request.Response.Header["Location"][0]，
// key 不存在就 index out of range，相对 Location 也不会被解析成绝对地址。
func GetHttpLocation(rawURL string) string {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return ""
	}
	return resolveLocation(rawURL, loc)
}

// resolveLocation 把可能是相对路径的 Location 解析成绝对地址。
func resolveLocation(base, loc string) string {
	b, err := url.Parse(base)
	if err != nil {
		return loc
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	return b.ResolveReference(ref).String()
}

// refreshLocationLoop 每 60 秒重新探测一次真实地址。
// 旧实现 recover 之后重开一个 goroutine，一旦反复 panic 就会脱离原来的生命周期。
func refreshLocationLoop(ctx context.Context, rawURL string) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if loc := GetHttpLocation(rawURL); loc != "" {
			setTargetURL(loc)
		} else {
			setTargetURL(rawURL)
		}
	}
}
