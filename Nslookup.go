package main

import (
	"net"
	"time"

	"github.com/miekg/dns"
)

// nslookup 返回 targetAddress 的地址列表，元素是纯 IP 字符串。
// 查 A 还是 AAAA 跟随当前地址族，状态栏显示的就是真正在用的那种地址。
func nslookup(targetAddress, server string) []string {
	qtype := uint16(dns.TypeA)
	if selectedFamily() == familyIPv6 {
		qtype = dns.TypeAAAA
	}
	return lookupByType(targetAddress, server, qtype)
}

// lookupByType 按指定记录类型查询。以前这里把整条资源记录
// （"name.\t3600\tIN\tA\t1.2.3.4"）当 IP 返回，还会把耗时打到 stdout 破坏 TUI。
func lookupByType(targetAddress, server string, qtype uint16) []string {
	if server == "" {
		server = "8.8.8.8"
	}
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}

	c := dns.Client{Timeout: 5 * time.Second}
	m := dns.Msg{}
	m.SetQuestion(dns.Fqdn(targetAddress), qtype)

	r, _, err := c.Exchange(&m, server)
	if err != nil || r == nil {
		return nil
	}

	var res []string
	for _, ans := range r.Answer {
		switch a := ans.(type) {
		case *dns.A:
			if a.A != nil {
				res = append(res, a.A.String())
			}
		case *dns.AAAA:
			if a.AAAA != nil {
				res = append(res, a.AAAA.String())
			}
		}
	}
	return res
}
