package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Subscribe 拉取订阅地址返回的目标 URL。加了超时和响应体上限，
// 订阅方挂住时不会把整个进程一起拖死。
func Subscribe(subURL string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(subURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "subscribe %s: %v\n", subURL, err)
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "subscribe %s: %v\n", subURL, err)
		return ""
	}
	return strings.TrimSpace(string(body))
}

// subscribeLoop 每 30 秒刷新一次订阅得到的目标地址。
func subscribeLoop(ctx context.Context, subURL string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next := Subscribe(subURL)
		if next == "" {
			continue
		}
		if *detectLocation {
			if loc := GetHttpLocation(next); loc != "" {
				setTargetURL(loc)
				continue
			}
		}
		setTargetURL(next)
	}
}
