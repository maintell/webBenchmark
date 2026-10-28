package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
)

const (
	letterIdxBits = 6
	letterIdxMask = 1<<letterIdxBits - 1
	letterIdxMax  = 63 / letterIdxBits
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// header 保存一条原始 -H 参数，真正的随机化留到每次请求时再做。
type header struct {
	key, value string
}

type headersList []header

func (h *headersList) String() string {
	if h == nil {
		return ""
	}
	return fmt.Sprint(*h)
}

func (h *headersList) IsCumulative() bool {
	return true
}

// Set 解析 -H 'Key:Value'。格式不对时返回错误，让 flag 直接报出来，
// 而不是像以前那样静默丢弃用户写的头。
func (h *headersList) Set(value string) error {
	res := strings.SplitN(value, ":", 2)
	if len(res) != 2 {
		return fmt.Errorf("want 'Key:Value', got %q", value)
	}
	key := strings.TrimSpace(res[0])
	if key == "" {
		return fmt.Errorf("header name is empty in %q", value)
	}
	*h = append(*h, header{key: key, value: strings.TrimSpace(res[1])})
	return nil
}

type ipArray []string

func (i *ipArray) String() string {
	if i == nil {
		return ""
	}
	return strings.Join(*i, ",")
}

func (i *ipArray) Set(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("empty address")
	}
	*i = append(*i, v)
	return nil
}

// RandStringBytesMaskImpr 返回 n 个随机字母；n <= 0 时返回空串。
// 使用 top-level 的 rand 函数，它自带锁，可被多个 goroutine 并发调用。
func RandStringBytesMaskImpr(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i, cache, remain := n-1, rand.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = rand.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return string(b)
}

// randomCount 识别 "Random<N>" 形式的占位符。
func randomCount(s string) (int, bool) {
	if !strings.HasPrefix(s, "Random") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "Random"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// randIntn 是 math/rand.Intn 的防御性包装，n<=0 时返回 0 而不是 panic。
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}

func generateRandomIPAddress() string {
	return fmt.Sprintf("%d.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256))
}
