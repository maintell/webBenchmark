//go:build !linux

package main

import (
	"net"
	"syscall"
)

// drainState 在 Linux 之外就是一块复用缓冲：没有 splice，省不掉内核→用户态的拷贝。
type drainState struct {
	buf []byte
}

func newDrainState() *drainState { return &drainState{} }

func (s *drainState) close() {}

// drainRaw 在 Linux 之外退化成"读进缓冲再丢掉"。
//
// 省掉了解密和 HTTP 解析，但省不掉 copy_to_user——这一步只有 splice 能免。
func drainRaw(_ syscall.RawConn, conn *net.TCPConn, st *drainState, sink drainSink) error {
	if st == nil {
		var scratch []byte
		return drainReadFallback(conn, &scratch, sink)
	}
	return drainReadFallback(conn, &st.buf, sink)
}
