//go:build !windows

package main

import "syscall"

// setRecvBuf 给这条连接设内核接收缓冲上限。
//
// 显式设置会关掉 Linux 的按 BDP 自动调优：高 RTT 链路（比如跨境 0.8 秒往返）
// 上每连接需要几 MB 才能跑满，压小了单流速率会被直接锁死。所以它只适合
// "内存比带宽更紧"的机器，默认不开启。
func setRecvBuf(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}
