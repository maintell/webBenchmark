package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// pipeTransport 把握手消息塞进内存管道，用来在不建 TCP 的情况下跑完整套握手。
type pipeTransport struct {
	out chan []byte
	in  chan []byte
}

func (p *pipeTransport) sendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.out <- data
	return nil
}

func (p *pipeTransport) recvJSON(v any) error {
	select {
	case data, ok := <-p.in:
		if !ok {
			return io.EOF
		}
		return json.Unmarshal(data, v)
	case <-time.After(5 * time.Second):
		return errors.New("握手超时")
	}
}

func newPipePair() (server *pipeTransport, client *pipeTransport) {
	c2s := make(chan []byte, 16)
	s2c := make(chan []byte, 16)
	return &pipeTransport{out: s2c, in: c2s}, &pipeTransport{out: c2s, in: s2c}
}

func testKey(seed byte) []byte {
	return bytes.Repeat([]byte{seed}, 32)
}

func TestSecureChannelRoundTrip(t *testing.T) {
	client, err := newSecureChannel(testKey(1), testKey(2), false)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newSecureChannel(testKey(1), testKey(2), true)
	if err != nil {
		t.Fatal(err)
	}

	frame := client.seal([]byte("client to server"))
	plain, err := server.open(frame)
	if err != nil {
		t.Fatalf("server open: %v", err)
	}
	if string(plain) != "client to server" {
		t.Fatalf("unexpected plaintext %q", plain)
	}

	back := server.seal([]byte("server to client"))
	plain, err = client.open(back)
	if err != nil {
		t.Fatalf("client open: %v", err)
	}
	if string(plain) != "server to client" {
		t.Fatalf("unexpected plaintext %q", plain)
	}

	// 篡改任意一个字节都必须被发现（GCM 认证失败）
	next := client.seal([]byte("third"))
	broken := append([]byte{}, next...)
	broken[len(broken)-1] ^= 0x01
	if _, err := server.open(broken); err == nil {
		t.Fatal("tampered frame must be rejected")
	}

	// 重放旧帧必须被序号检查挡住
	if _, err := server.open(frame); err == nil {
		t.Fatal("replayed frame must be rejected")
	}

	// 序号跳跃（相当于丢了一帧或有人注入）同样拒绝：
	// 让客户端连发两帧，只把后一帧交给服务端。
	_ = client.seal([]byte("lost"))
	skipped := client.seal([]byte("after the gap"))
	if _, err := server.open(skipped); err == nil {
		t.Fatal("skipping a frame must be rejected")
	}
}

func TestTranscriptBinding(t *testing.T) {
	base := wsTranscript(wsRoleServer, testKey(1), testKey(2), testKey(3), testKey(4), nil)
	variants := [][]byte{
		wsTranscript(wsRoleClient, testKey(1), testKey(2), testKey(3), testKey(4), nil),
		wsTranscript(wsRoleServer, testKey(9), testKey(2), testKey(3), testKey(4), nil),
		wsTranscript(wsRoleServer, testKey(1), testKey(2), testKey(3), testKey(9), nil),
		wsTranscript(wsRoleServer, testKey(1), testKey(2), testKey(3), testKey(4), testKey(5)),
	}
	for i, v := range variants {
		if bytes.Equal(base, v) {
			t.Fatalf("variant %d must produce a different transcript", i)
		}
	}
}

// enrollTestAgent 造一个尚未入网的 agent（不建立任何连接）。
func enrollTestAgent(t *testing.T, m *master, base, dir, name string) *agent {
	t.Helper()
	token, _, err := m.pki.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAgent(agentConfig{
		Dir: dir, URL: base, Name: name, Token: token, Fingerprint: m.pki.fp,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func TestHandshakeEnrollAndResume(t *testing.T) {
	m, _ := newTestMaster(t)
	dir := t.TempDir()
	a := enrollTestAgent(t, m, "https://127.0.0.1:1", dir, "n1")

	serverT, clientT := newPipePair()
	type result struct {
		sess *wsServerSession
		err  error
	}
	done := make(chan result, 1)
	go func() {
		sess, err := m.handshakeServer(serverT, "127.0.0.1")
		done <- result{sess, err}
	}()

	channel, err := a.handshakeClient(clientT)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("server handshake: %v", res.err)
	}
	if !res.sess.Enrolled {
		t.Fatal("an agent without identity should enrol")
	}

	// 主控侧发出 ready（含新证书），节点侧解密并落盘
	ready := wsControl{T: "ready", NodeID: res.sess.NodeID, Cert: string(res.sess.CertPEM)}
	plain, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	frame := res.sess.Channel.seal(plain)
	decoded, err := channel.open(frame)
	if err != nil {
		t.Fatalf("client could not open the ready frame: %v", err)
	}
	var got wsControl
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := a.applyReady(got); err != nil {
		t.Fatalf("applyReady: %v", err)
	}
	if !a.hasIdentity() {
		t.Fatal("agent should have persisted its identity")
	}
	if a.state.NodeID != res.sess.NodeID {
		t.Fatalf("node id mismatch: %s vs %s", a.state.NodeID, res.sess.NodeID)
	}

	// 重启：从磁盘读回身份，走的应该是 resume 而不是再入网
	again, err := newAgent(agentConfig{Dir: dir, URL: "https://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	serverT2, clientT2 := newPipePair()
	done2 := make(chan result, 1)
	go func() {
		sess, err := m.handshakeServer(serverT2, "127.0.0.1")
		done2 <- result{sess, err}
	}()
	if _, err := again.handshakeClient(clientT2); err != nil {
		t.Fatalf("resume handshake: %v", err)
	}
	res2 := <-done2
	if res2.err != nil {
		t.Fatalf("server resume: %v", res2.err)
	}
	if res2.sess.Enrolled {
		t.Fatal("a reloaded agent must not enrol again")
	}
	if res2.sess.NodeID != a.state.NodeID {
		t.Fatalf("identity changed across restart: %s", res2.sess.NodeID)
	}
}

func TestHandshakeRejectsWrongFingerprint(t *testing.T) {
	m, _ := newTestMaster(t)
	a := enrollTestAgent(t, m, "https://127.0.0.1:1", t.TempDir(), "n1")
	// 伪造一枚指纹：服务端签名用的是真 CA 私钥，节点这边必须对不上
	a.cfg.Fingerprint = strings.Repeat("ab", 32)

	serverT, clientT := newPipePair()
	go func() { _, _ = m.handshakeServer(serverT, "127.0.0.1") }()
	if _, err := a.handshakeClient(clientT); err == nil {
		t.Fatal("handshake must fail when the CA fingerprint does not match")
	}
}

func TestHandshakeTokenIsSingleUse(t *testing.T) {
	m, _ := newTestMaster(t)
	a := enrollTestAgent(t, m, "https://127.0.0.1:1", t.TempDir(), "n1")

	serverT, clientT := newPipePair()
	done := make(chan error, 1)
	go func() {
		_, err := m.handshakeServer(serverT, "127.0.0.1")
		done <- err
	}()
	if _, err := a.handshakeClient(clientT); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side: %v", err)
	}

	// 同一个令牌换个目录再来一次：必须被拒绝，否则一枚令牌能入网两台机器。
	a2, err := newAgent(agentConfig{
		Dir: t.TempDir(), URL: "https://127.0.0.1:1", Name: "n2",
		Token: a.cfg.Token, Fingerprint: m.pki.fp,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverT2, clientT2 := newPipePair()
	go func() { _, _ = m.handshakeServer(serverT2, "127.0.0.1") }()
	_, handshakeErr := a2.handshakeClient(clientT2)
	if handshakeErr == nil {
		t.Fatal("a join token must only be usable once")
	}
	if !strings.Contains(handshakeErr.Error(), "已被使用") {
		t.Fatalf("unexpected error: %v", handshakeErr)
	}
}
