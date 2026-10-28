package main

import (
	"net"
	"time"

	"github.com/miekg/dns"
)

// nslookup 返回 targetAddress 的 A 记录，元素是纯 IP 字符串。
// 以前这里把整条资源记录（"name.\t3600\tIN\tA\t1.2.3.4"）当 IP 返回，
// 而且会把查询耗时直接打到 stdout 破坏 TUI。
func nslookup(targetAddress, server string) []string {
	if server == "" {
		server = "8.8.8.8"
	}
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}

	c := dns.Client{Timeout: 5 * time.Second}
	m := dns.Msg{}
	m.SetQuestion(dns.Fqdn(targetAddress), dns.TypeA)

	r, _, err := c.Exchange(&m, server)
	if err != nil || r == nil {
		return nil
	}

	var res []string
	for _, ans := range r.Answer {
		a, ok := ans.(*dns.A)
		if !ok || a.A == nil {
			continue
		}
		res = append(res, a.A.String())
	}
	return res
}
