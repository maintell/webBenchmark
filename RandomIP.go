package main

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const countryCIDRBase = "https://cdn.jsdelivr.net/gh/maintell/country-ip-blocks@master/ipv4/"

var (
	cidrOnce sync.Once
	warnOnce sync.Once

	cidrMu   sync.RWMutex
	cidrList []string
)

// preloadCountryCIDR 在后台把国家 CIDR 列表拉下来。
//
// 这一步绝对不能放在请求路径上：以前是请求里判断 "列表为空就去下载"，
// 下载慢的时候所有 goroutine 一起堵在 fetch 上，实测默认参数下 4 秒 0 请求；
// 下载失败时还会每个请求重试一次，把压测彻底拖垮。
func preloadCountryCIDR(country string) {
	cidrOnce.Do(func() {
		list, err := fetchCountryCIDR(country)
		if err != nil || len(list) == 0 {
			warnOnce.Do(func() {
				fmt.Fprintf(os.Stderr, "warning: country ip list for %q is unavailable (%v), falling back to random addresses\n", country, err)
			})
			return
		}
		cidrMu.Lock()
		cidrList = list
		cidrMu.Unlock()
	})
}

func fetchCountryCIDR(country string) ([]string, error) {
	src := countryCIDRBase + strings.ToLower(country) + ".cidr"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var list []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 只收合法 IPv4 段。jsdelivr 对不存在的国家会返回 200 加一页 HTML，
		// 以前这种脏数据会让后面的解析直接 nil 解引用崩掉。
		if _, ipNet, err := net.ParseCIDR(line); err != nil || ipNet.IP.To4() == nil {
			continue
		}
		list = append(list, line)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no usable ipv4 cidr in %s", src)
	}
	return list, nil
}

// randomForwardedIP 返回一个随机 IPv4，永不阻塞。
// 国家列表还没加载完（或加载失败）时直接回退到完全随机地址。
func randomForwardedIP() string {
	cidrMu.RLock()
	list := cidrList
	cidrMu.RUnlock()

	if len(list) > 0 {
		if ip, err := GenerateRandomIPFromCIDR(list[rand.Intn(len(list))]); err == nil {
			return ip
		}
	}
	return generateRandomIPAddress()
}

// GenerateRandomIPFromCIDR 在给定网段内均匀取一个 IPv4 地址。
// 掩码按位处理，所以 /20 这类非 8 位对齐的网段也能覆盖整个范围，
// 而不是像以前那样只在最后一个字节里随机。
func GenerateRandomIPFromCIDR(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid cidr %q: %w", cidr, err)
	}
	ip := ipNet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("cidr %q is not an IPv4 network", cidr)
	}
	mask := ipNet.Mask
	if len(mask) != net.IPv4len {
		return "", fmt.Errorf("cidr %q has an unexpected mask length %d", cidr, len(mask))
	}
	out := make(net.IP, net.IPv4len)
	for i := 0; i < net.IPv4len; i++ {
		out[i] = ip[i] | byte(rand.Intn(256))&^mask[i]
	}
	return out.String(), nil
}

// RemoveEmptyItem 去掉空串元素。
func RemoveEmptyItem(arr []string) []string {
	var result []string
	for _, str := range arr {
		if str != "" {
			result = append(result, str)
		}
	}
	return result
}
