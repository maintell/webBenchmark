package main

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// agentConn 是一条已建立的主从会话。
//
// 写端只有一个协程：业务侧把控制帧丢进 out，由 writer 统一加密后发送。
// 这样任务派发、看门狗停止、心跳 ping 可以并发投递，而上层锁里不会出现阻塞写。
type agentConn struct {
	nodeID string
	remote string
	ws     *wsConn
	ch     *secureChannel
	out    chan wsControl
	done   chan struct{}
	once   sync.Once
}

func (c *agentConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.ws.close()
	})
}

// send 投递一条控制帧。队列满或连接已关闭都返回 false，
// 调用方据此把该分片标记成 lost，而不是让消息悄悄消失。
func (c *agentConn) send(msg wsControl) bool {
	select {
	case c.out <- msg:
		return true
	case <-c.done:
		return false
	default:
		c.close()
		return false
	}
}

func (c *agentConn) writer() {
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.out:
			plain, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if err := c.ws.sendBinary(c.ch.seal(plain)); err != nil {
				c.close()
				return
			}
		}
	}
}

// keepalive 定期下发加密 ping：从节点据此知道自己没有被半开连接卡住。
func (c *agentConn) keepalive() {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if !c.send(wsControl{T: "ping"}) {
				return
			}
		}
	}
}

// handleAgentWS 是主从之间的唯一入口：握手、加密、命令下发、进度回收都在这里。
func (m *master) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.close()

	ip := clientIP(r)
	session, err := m.handshakeServer(ws, ip)
	if err != nil {
		m.audit.event("agent_handshake_failed", map[string]any{"ip": ip, "reason": err.Error()})
		return
	}

	conn := &agentConn{
		nodeID: session.NodeID,
		remote: ip,
		ws:     ws,
		ch:     session.Channel,
		out:    make(chan wsControl, 64),
		done:   make(chan struct{}),
	}
	defer conn.close()

	// 第一条加密帧就是 ready：入网时它捎带刚签发的节点证书。
	ready := wsControl{T: "ready", NodeID: session.NodeID}
	if session.Enrolled {
		ready.Cert = string(session.CertPEM)
	}
	plain, err := json.Marshal(ready)
	if err != nil {
		return
	}
	if err := ws.sendBinary(session.Channel.seal(plain)); err != nil {
		return
	}

	m.attachConn(session, conn)
	defer m.detachConn(session.NodeID, conn)

	go conn.writer()
	go conn.keepalive()

	for {
		kind, data, err := ws.readMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		frame, err := session.Channel.open(data)
		if err != nil {
			m.audit.event("agent_frame_rejected", map[string]any{
				"node": session.NodeID, "reason": err.Error(),
			})
			return
		}
		msg, err := decodeControl(frame)
		if err != nil {
			continue
		}
		m.handleAgentControl(session.NodeID, conn, msg)
	}
}

func (m *master) handleAgentControl(nodeID string, conn *agentConn, msg wsControl) {
	m.mu.Lock()
	if n := m.nodes[nodeID]; n != nil {
		n.lastSeen = time.Now()
	}
	m.mu.Unlock()

	switch msg.T {
	case "heartbeat":
		m.touchNode(nodeID, msg.HB, conn.remote)
	case "progress":
		if msg.P != nil {
			m.applyProgress(nodeID, *msg.P)
		}
	case "result":
		if msg.R != nil {
			m.applyResult(nodeID, *msg.R)
		}
	case "renew":
		m.handleRenew(nodeID, conn, msg.CSR)
	case "pong":
		// 心跳已在上面续过 lastSeen，这里不需要额外处理。
	}
}

func (m *master) handleRenew(nodeID string, conn *agentConn, csr string) {
	out := wsControl{T: "renewed"}
	certPEM, serial, err := m.pki.issueClientCert(csr, nodeID)
	if err != nil {
		m.audit.event("renew_rejected", map[string]any{"node": nodeID, "reason": err.Error()})
		out.Error = "CSR 无效：" + err.Error()
		conn.send(out)
		return
	}
	m.mu.Lock()
	if n := m.nodes[nodeID]; n != nil {
		n.serial = serial
		m.persistLocked()
	}
	m.mu.Unlock()
	m.audit.event("renew", map[string]any{"node": nodeID, "serial": serial})
	out.Cert = string(certPEM)
	conn.send(out)
}

func (m *master) touchNode(nodeID string, hb *wsHeartbeat, ip string) {
	if hb == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[nodeID]
	if n == nil {
		return
	}
	n.lastSeen = time.Now()
	n.remoteAddr = ip
	n.running = sanitizeNodeName(hb.Running)
	if v := sanitizeNodeName(hb.Version); v != "" {
		n.version = v
	}
	if v := sanitizeNodeName(hb.OS); v != "" {
		n.os = v
	}
	if v := sanitizeNodeName(hb.Arch); v != "" {
		n.arch = v
	}
	if hb.CPUs > 0 {
		n.cpus = hb.CPUs
	}
}

// attachConn 把新会话挂到节点上。同一节点重复连接时，旧会话立刻关闭：
// 一个身份同时只有一条活跃通道，避免两条连接各自收到一半命令。
func (m *master) attachConn(session *wsServerSession, conn *agentConn) {
	m.mu.Lock()
	n := m.nodes[session.NodeID]
	if n == nil {
		m.mu.Unlock()
		conn.close()
		return
	}
	old := n.conn
	n.conn = conn
	n.lastSeen = time.Now()
	n.remoteAddr = conn.remote
	if v := sanitizeNodeName(session.Hello.Version); v != "" {
		n.version = v
	}
	if v := sanitizeNodeName(session.Hello.OS); v != "" {
		n.os = v
	}
	if v := sanitizeNodeName(session.Hello.Arch); v != "" {
		n.arch = v
	}
	if session.Hello.CPUs > 0 {
		n.cpus = session.Hello.CPUs
	}
	m.mu.Unlock()

	if old != nil {
		old.close()
	}
	if session.Enrolled {
		distLogf("master: 节点 %s（%s）已加入，来自 %s", session.NodeID,
			sanitizeNodeName(session.Hello.Name), conn.remote)
	}
}

func (m *master) detachConn(nodeID string, conn *agentConn) {
	m.mu.Lock()
	if n := m.nodes[nodeID]; n != nil && n.conn == conn {
		n.conn = nil
	}
	m.mu.Unlock()
}

// closeAgentConns 在停机时主动断开所有会话。
// http.Server.Shutdown 不会关闭已劫持（hijacked）的连接，不主动断开会卡住退出。
func (m *master) closeAgentConns() {
	m.mu.Lock()
	conns := make([]*agentConn, 0, len(m.nodes))
	for _, n := range m.nodes {
		if n.conn != nil {
			conns = append(conns, n.conn)
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// enqueueLocked 把控制帧投递给节点。调用方必须持有 m.mu。
func (m *master) enqueueLocked(nodeID string, msg wsControl) bool {
	n := m.nodes[nodeID]
	if n == nil || n.revoked || n.conn == nil {
		return false
	}
	return n.conn.send(msg)
}

// precheckEnrollToken 在握手早期做一次快速校验：令牌无效就不必继续协商密钥。
// 真正的"占位"发生在签发证书那一刻，保证单次使用不会被执行两次并发请求绕过。
func (m *master) precheckEnrollToken(token, ip string) error {
	// 入网是唯一不需要既有身份的入口，单独按来源限流。
	if !m.enroll.allow(ip) {
		return errors.New("入网请求过于频繁，请稍后再试")
	}
	pl, err := m.pki.verifyJoinToken(token)
	if err != nil {
		m.audit.event("enroll_rejected", map[string]any{"ip": ip, "reason": err.Error()})
		return errors.New("加入令牌无效或已过期，请在主控控制台重新签发")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, used := m.usedTokens[pl.Nonce]; used {
		m.audit.event("enroll_replay", map[string]any{"ip": ip})
		return errors.New("加入令牌已被使用")
	}
	if len(m.nodes) >= m.maxNodes {
		return errors.New("节点数量已达上限")
	}
	return nil
}

// precheckResume 校验老节点的证书：只认本 CA 签发、与身份匹配、未吊销、序列号当前。
func (m *master) precheckResume(nodeID, certPEM string) (*x509.Certificate, error) {
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(certPEM) == "" {
		return nil, errors.New("缺少节点身份")
	}
	cert, err := parseCertPEM([]byte(certPEM))
	if err != nil {
		return nil, errors.New("节点证书无法解析")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     m.pki.caPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, errors.New("节点证书不是本主控签发的")
	}
	if cert.Subject.CommonName != nodeID {
		return nil, errors.New("证书与节点身份不一致")
	}
	if !time.Now().Before(cert.NotAfter) {
		return nil, errors.New("节点证书已过期，请重新加入")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[nodeID]
	if n == nil {
		return nil, errors.New("节点未注册，请重新加入集群")
	}
	if n.revoked {
		return nil, errors.New("节点已被吊销")
	}
	// 序列号对不上说明这张证书已经被换发过（或被人复制到了别处）。
	if n.serial != cert.SerialNumber.Text(16) {
		return nil, errors.New("证书已失效，请重新加入集群")
	}
	return cert, nil
}

// issueEnrolledNode 消耗一次性令牌、签发证书并登记节点。
func (m *master) issueEnrolledNode(hello wsHello, ip string) (certPEM []byte, nodeID string, cert *x509.Certificate, err error) {
	if strings.TrimSpace(hello.CSR) == "" {
		return nil, "", nil, errors.New("入网请求缺少 CSR")
	}
	pl, err := m.pki.verifyJoinToken(hello.Token)
	if err != nil {
		return nil, "", nil, errors.New("加入令牌无效或已过期")
	}

	// 先占住 nonce 再签发；签发失败再释放。同一个临界区里完成，
	// 两个并发握手就不可能拿同一个令牌各自换到一张证书。
	now := time.Now()
	m.mu.Lock()
	if _, used := m.usedTokens[pl.Nonce]; used {
		m.mu.Unlock()
		return nil, "", nil, errors.New("加入令牌已被使用")
	}
	if len(m.nodes) >= m.maxNodes {
		m.mu.Unlock()
		return nil, "", nil, errors.New("节点数量已达上限")
	}
	m.usedTokens[pl.Nonce] = pl.Exp
	m.pruneTokensLocked(now)
	m.mu.Unlock()

	nodeID = "node-" + randomHex(4)
	certPEM, serial, err := m.pki.issueClientCert(hello.CSR, nodeID)
	if err != nil {
		m.mu.Lock()
		delete(m.usedTokens, pl.Nonce)
		m.mu.Unlock()
		m.audit.event("enroll_rejected", map[string]any{"ip": ip, "reason": err.Error()})
		return nil, "", nil, fmt.Errorf("CSR 无效：%w", err)
	}
	cert, err = parseCertPEM(certPEM)
	if err != nil {
		return nil, "", nil, err
	}

	m.mu.Lock()
	m.nodes[nodeID] = &nodeRuntime{
		id:         nodeID,
		name:       sanitizeNodeName(hello.Name),
		serial:     serial,
		enrolledAt: now,
		lastSeen:   now,
		version:    sanitizeNodeName(hello.Version),
		os:         sanitizeNodeName(hello.OS),
		arch:       sanitizeNodeName(hello.Arch),
		cpus:       hello.CPUs,
		remoteAddr: ip,
	}
	m.persistLocked()
	m.mu.Unlock()

	m.audit.event("enroll", map[string]any{
		"node": nodeID, "name": sanitizeNodeName(hello.Name), "ip": ip,
		"version": hello.Version, "serial": serial,
	})
	return certPEM, nodeID, cert, nil
}
