//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// spliceChunk 是一次搬运的长度上限，实际受 pipe 容量限制（下面调到 256KB）。
const spliceChunk = 1 << 20

// drainToNull 是 /dev/null 的写端。它的 fops 带 splice_write，可以直接作为落点；
// 进程内只开一次。
//
// 必须保留 *os.File 本体：只留 Fd() 的话，这个对象一旦不可达，GC 的 finalizer
// 会把 fd 关掉，之后 splice 就会莫名其妙地 EBADF/EINVAL（第一版正是这么踩的，
// 而且现象是"搬了几百 KB 就卡死"，很难往这里想）。
var drainToNull = func() *os.File {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	return f
}()

var drainToNullFD = func() int {
	if drainToNull == nil {
		return -1
	}
	return int(drainToNull.Fd())
}()

// drainState 在 Linux 上是"一条中转 pipe"。
//
// splice 要求至少一端是 pipe，所以不能把 socket 直接接到 /dev/null：
// 正确做法是 socket → pipe → /dev/null，全程只动页引用，不拷数据。
type drainState struct {
	pipeR int
	pipeW int
	buf   []byte // 没有 pipe 时的回退缓冲
}

func newDrainState() *drainState {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_CLOEXEC); err != nil {
		return &drainState{pipeR: -1, pipeW: -1}
	}
	// 把中转 pipe 调大，减少"socket→pipe→null"的往返次数。
	_, _ = unix.FcntlInt(uintptr(fds[1]), unix.F_SETPIPE_SZ, 256<<10)
	return &drainState{pipeR: fds[0], pipeW: fds[1]}
}

func (s *drainState) close() {
	if s == nil {
		return
	}
	if s.pipeR >= 0 && s.pipeW >= 0 {
		unix.Close(s.pipeR)
		unix.Close(s.pipeW)
	}
	s.pipeR, s.pipeW = -1, -1
}

// drainRaw 用 splice 把内核接收缓冲经 pipe 搬进 /dev/null。
//
// 必须先排空判据再返回：Go 的 netpoller 是边缘触发，回调提前 return true 会让
// 后续数据不再产生 wakeup（第一版就是这么把自己卡在几百 KB/s 的）。
// 所以这里一直搬到自己 EAGAIN 为止，字节数通过 sink 实时上报。
func drainRaw(rc syscall.RawConn, conn *net.TCPConn, st *drainState, sink drainSink) error {
	if st == nil || st.pipeW < 0 || drainToNullFD < 0 {
		var scratch []byte
		if st != nil {
			scratch = st.buf
		}
		err := drainReadFallback(conn, &scratch, sink)
		if st != nil {
			st.buf = scratch
		}
		return err
	}

	var opErr error
	err := rc.Read(func(fd uintptr) bool {
		for {
			n, e := unix.Splice(int(fd), nil, st.pipeW, nil, spliceChunk, unix.SPLICE_F_MOVE)
			if e != nil && !errors.Is(e, unix.EAGAIN) {
				e = fmt.Errorf("splice sock->pipe: %w", e)
			}
			// unix.Splice 在 32 位平台（linux/arm）返回 int，在 amd64 返回 int64，
			// 这里统一转成 int64 再算，否则交叉编译到 arm 会直接编译不过。
			if got := int64(n); got > 0 {
				// 必须立刻排空 pipe，否则下一次 splice 会一直 EAGAIN。
				for moved := int64(0); moved < got; {
					k, ke := unix.Splice(st.pipeR, nil, drainToNullFD, nil, int(got-moved), 0)
					if ke != nil && !errors.Is(ke, unix.EAGAIN) {
						ke = fmt.Errorf("splice pipe->null: %w", ke)
					}
					if k <= 0 {
						if ke != nil && !errors.Is(ke, unix.EAGAIN) {
							opErr = ke
							return true
						}
						break
					}
					moved += int64(k)
				}
				if sink != nil {
					if se := sink(got); se != nil {
						opErr = se
						return true
					}
				}
				continue
			}
			switch {
			case e == nil: // n == 0 且无错误：对端关闭
				opErr = io.EOF
				return true
			case errors.Is(e, unix.EAGAIN):
				return false
			default:
				opErr = e
				return true
			}
		}
	})
	if err != nil {
		return err
	}
	return opErr
}
