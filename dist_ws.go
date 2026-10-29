package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsReadLimit  = 1 << 20
	wsWriteWait  = 15 * time.Second
	wsIdleWait   = 60 * time.Second
	wsPingPeriod = 15 * time.Second
)

// wsConn 把 gorilla 的连接包成"一条消息一次写"的包装。
// gorilla 不允许并发写，这里用互斥锁串行化；读端始终只有一个读者。
type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) sendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.send(websocket.TextMessage, data)
}

func (w *wsConn) sendBinary(b []byte) error {
	return w.send(websocket.BinaryMessage, b)
}

func (w *wsConn) send(kind int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return w.c.WriteMessage(kind, payload)
}

// recvJSON 只用于握手阶段（明文 JSON 帧）。
func (w *wsConn) recvJSON(v any) error {
	_, data, err := w.readMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// readMessage 每次读都续一次空闲期限：有消息就说明链路活着。
func (w *wsConn) readMessage() (int, []byte, error) {
	_ = w.c.SetReadDeadline(time.Now().Add(wsIdleWait))
	return w.c.ReadMessage()
}

func (w *wsConn) close() {
	_ = w.c.Close()
}

// wsUpgrader 只接受非浏览器来源。
// 控制面靠令牌与证书认证，带上 Origin 的请求一律拒绝，
// 这样即使有人在恶意页面里写 new WebSocket(...)，也拿不到升级机会。
var wsUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   4096,
	WriteBufferSize:  4096,
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == ""
	},
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	c, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(wsReadLimit)
	return &wsConn{c: c}, nil
}

// wsDial 连接主控。tlsConf 为 nil 时走明文 ws://。
func wsDial(ctx context.Context, rawURL string, tlsConf *tls.Config) (*wsConn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		TLSClientConfig:   tlsConf,
		EnableCompression: false,
	}
	c, resp, err := dialer.DialContext(ctx, rawURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("主控拒绝了连接（HTTP %d）: %w", resp.StatusCode, err)
		}
		return nil, err
	}
	c.SetReadLimit(wsReadLimit)
	return &wsConn{c: c}, nil
}
