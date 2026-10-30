//go:build windows

package main

import "syscall"

// setRecvBuf 给这条连接设内核接收缓冲上限（Windows 走 SO_RCVBUF）。
// 语义与 unix 版本一致：显式设置会关掉自动调优，只在内存紧张的机器上才值得开。
func setRecvBuf(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}
