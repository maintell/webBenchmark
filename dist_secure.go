package main

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// 主从通道的加密层。
//
// 为什么不用 mTLS 而自己握手：主控常常挂在 nginx / caddy 后面，TLS 在代理上就被
// 终止了，客户端证书未必能到进程里；主控改成纯 http 时更是连 TLS 都没有。
// 把身份与加密下沉到应用层后，两种 scheme 下都是同一套保证：
//
//	服务端用主控 CA 私钥对握手摘要签名 -> 节点只要一枚 CA 指纹就能确认对端
//	节点用 CA 签发的客户端证书对同一份摘要签名 -> 主控确认这台机器就是那个节点
//	X25519 临时密钥 + HKDF 得到双向会话密钥 -> 之后每一帧都是 AES-256-GCM
//
// 这里只做"认证 + 密钥协商"，不重造 TLS 记录层：帧序号严格递增，
// 序号不连续或解密失败一律断开，重放与乱序都会当场暴露。
const (
	wsProtoVersion = 1
	wsHandshakeTag = "webBenchmark-ws-v1"

	wsRoleServer = 0x01
	wsRoleClient = 0x02
)

// 握手期消息全部是公开数据：nonce、临时公钥、CA 证书、签名。
type wsHello struct {
	T       string `json:"t"`
	V       int    `json:"v"`
	Mode    string `json:"mode"` // enroll | resume
	Nonce   string `json:"nonce"`
	Eph     string `json:"eph"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
	CPUs    int    `json:"cpus,omitempty"`
	Token   string `json:"token,omitempty"`
	CSR     string `json:"csr,omitempty"`
	NodeID  string `json:"nodeId,omitempty"`
	Cert    string `json:"cert,omitempty"`
}

type wsChallenge struct {
	T     string `json:"t"`
	Nonce string `json:"nonce,omitempty"`
	Eph   string `json:"eph,omitempty"`
	CA    string `json:"ca,omitempty"`
	Sig   string `json:"sig,omitempty"`
	Error string `json:"error,omitempty"`
}

type wsAuth struct {
	T   string `json:"t"`
	Sig string `json:"sig,omitempty"`
}

// wsError 只用于会话建立之前的失败，让节点侧能给出人话。
type wsError struct {
	T     string `json:"t"`
	Error string `json:"error"`
}

// wsControl 是通道建立之后的控制帧，命令、进度、结果、心跳、换证、就绪共用一个信封。
type wsControl struct {
	T      string       `json:"t"`
	NodeID string       `json:"nodeId,omitempty"`
	Spec   *TaskSpec    `json:"spec,omitempty"`
	TaskID string       `json:"taskId,omitempty"`
	Reason string       `json:"reason,omitempty"` // deadline | stopped
	P      *Progress    `json:"p,omitempty"`
	R      *TaskResult  `json:"r,omitempty"`
	HB     *wsHeartbeat `json:"hb,omitempty"`
	CSR    string       `json:"csr,omitempty"`
	Cert   string       `json:"cert,omitempty"`
	Error  string       `json:"error,omitempty"`
}

type wsHeartbeat struct {
	Running string `json:"running,omitempty"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
	CPUs    int    `json:"cpus,omitempty"`
}

// handshakeTransport 抽象"收发一条 JSON 消息"，测试里用内存管道就能跑完整个握手。
type handshakeTransport interface {
	sendJSON(v any) error
	recvJSON(v any) error
}

// secureChannel 是握手完成后的 AEAD 通道。
// 两个方向各一把密钥，帧序号严格递增。
type secureChannel struct {
	send    cipher.AEAD
	recv    cipher.AEAD
	sendCtr uint64
	recvCtr uint64
}

func newSecureChannel(kC2S, kS2C []byte, isServer bool) (*secureChannel, error) {
	if len(kC2S) != 32 || len(kS2C) != 32 {
		return nil, errors.New("会话密钥长度必须是 32 字节")
	}
	sendKey, recvKey := kC2S, kS2C
	if isServer {
		sendKey, recvKey = kS2C, kC2S
	}
	sendBlock, err := aes.NewCipher(sendKey)
	if err != nil {
		return nil, err
	}
	recvBlock, err := aes.NewCipher(recvKey)
	if err != nil {
		return nil, err
	}
	sendAEAD, err := cipher.NewGCM(sendBlock)
	if err != nil {
		return nil, err
	}
	recvAEAD, err := cipher.NewGCM(recvBlock)
	if err != nil {
		return nil, err
	}
	return &secureChannel{send: sendAEAD, recv: recvAEAD}, nil
}

// seal 产出一帧：[8 字节序号][密文+tag]，序号同时作为附加认证数据。
func (c *secureChannel) seal(plain []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], c.sendCtr)
	nonce := make([]byte, 12)
	copy(nonce[4:], hdr[:])
	out := make([]byte, 0, 8+len(plain)+c.send.Overhead())
	out = append(out, hdr[:]...)
	out = c.send.Seal(out, nonce, plain, hdr[:])
	c.sendCtr++
	return out
}

// open 校验序号并解密。序号必须正好等于期望值：底层是可靠有序流，
// 出现跳跃只有两种可能——丢帧，或者有人在注入旧帧。两种都不能继续。
func (c *secureChannel) open(frame []byte) ([]byte, error) {
	if len(frame) < 8+c.recv.Overhead() {
		return nil, errors.New("帧长度不足")
	}
	hdr := frame[:8]
	ctr := binary.BigEndian.Uint64(hdr)
	if ctr != c.recvCtr {
		return nil, fmt.Errorf("帧序号不连续（期望 %d，收到 %d）", c.recvCtr, ctr)
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], hdr)
	plain, err := c.recv.Open(nil, nonce, frame[8:], hdr)
	if err != nil {
		return nil, errors.New("解密失败")
	}
	c.recvCtr++
	return plain, nil
}

// wsTranscript 把握手要素压成 32 字节摘要。
// 每个字段都带长度前缀，避免"两组不同字段拼起来恰好相同"的歧义。
func wsTranscript(role byte, nonceA, nonceM, ephA, ephM, certDER []byte) []byte {
	h := sha256.New()
	h.Write([]byte(wsHandshakeTag))
	h.Write([]byte{role})
	for _, part := range [][]byte{nonceA, nonceM, ephA, ephM, certDER} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(part)))
		h.Write(l[:])
		h.Write(part)
	}
	return h.Sum(nil)
}

func wsSessionKeys(shared, nonceA, nonceM []byte) (kC2S, kS2C []byte, err error) {
	salted := append(append([]byte{}, nonceA...), nonceM...)
	salt := sha256.Sum256(salted)
	buf := make([]byte, 64)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, salt[:], []byte(wsHandshakeTag+"-keys")), buf); err != nil {
		return nil, nil, err
	}
	return buf[:32], buf[32:], nil
}

func signDigest(signer crypto.Signer, digest []byte) ([]byte, error) {
	return signer.Sign(rand.Reader, digest, crypto.SHA256)
}

func verifyDigest(pub crypto.PublicKey, digest, sig []byte) error {
	switch key := pub.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, digest, sig) {
			return errors.New("签名不匹配")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest, sig); err != nil {
			return errors.New("签名不匹配")
		}
	default:
		return fmt.Errorf("不支持的公钥类型 %T", pub)
	}
	return nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// wsServerSession 是握手成功后主控侧的会话信息。
type wsServerSession struct {
	NodeID   string
	CertPEM  []byte
	Channel  *secureChannel
	Hello    wsHello
	Cert     *x509.Certificate
	Enrolled bool
}

// handshakeServer 执行主控侧握手，失败时把原因以明文回给节点（此时还没有会话密钥）。
func (m *master) handshakeServer(t handshakeTransport, remoteIP string) (*wsServerSession, error) {
	session, err := m.handshakeServerInner(t, remoteIP)
	if err != nil {
		_ = t.sendJSON(wsError{T: "error", Error: err.Error()})
	}
	return session, err
}

func (m *master) handshakeServerInner(t handshakeTransport, remoteIP string) (*wsServerSession, error) {
	var hello wsHello
	if err := t.recvJSON(&hello); err != nil {
		return nil, fmt.Errorf("读取握手请求失败: %w", err)
	}
	if hello.T != "hello" {
		return nil, errors.New("握手顺序错误")
	}
	if hello.V != wsProtoVersion {
		return nil, fmt.Errorf("协议版本不一致：主控 %d，节点 %d，请让主从使用同一版本的可执行文件", wsProtoVersion, hello.V)
	}
	nonceA, err := unb64(hello.Nonce)
	if err != nil || len(nonceA) != 32 {
		return nil, errors.New("握手 nonce 不合法")
	}
	ephABytes, err := unb64(hello.Eph)
	if err != nil || len(ephABytes) != 32 {
		return nil, errors.New("握手临时公钥不合法")
	}

	var clientCert *x509.Certificate
	switch hello.Mode {
	case "enroll":
		if err := m.precheckEnrollToken(hello.Token, remoteIP); err != nil {
			return nil, err
		}
	case "resume":
		cert, err := m.precheckResume(hello.NodeID, hello.Cert)
		if err != nil {
			return nil, err
		}
		clientCert = cert
	default:
		return nil, errors.New("未知的握手模式")
	}

	nonceM, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	curve := ecdh.X25519()
	ephS, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ephSBytes := ephS.PublicKey().Bytes()

	serverDigest := wsTranscript(wsRoleServer, nonceA, nonceM, ephABytes, ephSBytes, nil)
	serverSig, err := signDigest(m.pki.key, serverDigest)
	if err != nil {
		return nil, err
	}
	if err := t.sendJSON(wsChallenge{
		T: "challenge", Nonce: b64(nonceM), Eph: b64(ephSBytes),
		CA: string(m.pki.certPEM), Sig: b64(serverSig),
	}); err != nil {
		return nil, err
	}

	var auth wsAuth
	if err := t.recvJSON(&auth); err != nil {
		return nil, fmt.Errorf("读取握手响应失败: %w", err)
	}
	if auth.T != "auth" {
		return nil, errors.New("握手顺序错误")
	}
	if hello.Mode == "resume" {
		sig, err := unb64(auth.Sig)
		if err != nil {
			return nil, errors.New("握手签名不合法")
		}
		clientDigest := wsTranscript(wsRoleClient, nonceA, nonceM, ephABytes, ephSBytes, clientCert.Raw)
		if err := verifyDigest(clientCert.PublicKey, clientDigest, sig); err != nil {
			return nil, fmt.Errorf("节点未能证明私钥持有: %w", err)
		}
	}

	peerPub, err := curve.NewPublicKey(ephABytes)
	if err != nil {
		return nil, errors.New("握手临时公钥不合法")
	}
	shared, err := ephS.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("密钥协商失败: %w", err)
	}
	kC2S, kS2C, err := wsSessionKeys(shared, nonceA, nonceM)
	if err != nil {
		return nil, err
	}
	channel, err := newSecureChannel(kC2S, kS2C, true)
	if err != nil {
		return nil, err
	}

	session := &wsServerSession{Channel: channel, Hello: hello, Cert: clientCert}
	if hello.Mode == "enroll" {
		certPEM, nodeID, cert, err := m.issueEnrolledNode(hello, remoteIP)
		if err != nil {
			return nil, err
		}
		session.NodeID = nodeID
		session.CertPEM = certPEM
		session.Cert = cert
		session.Enrolled = true
	} else {
		session.NodeID = hello.NodeID
	}
	return session, nil
}

// handshakeClient 执行节点侧握手并返回会话密钥。
//
// 首次入网时手里只有一枚 CA 指纹：先比对链根指纹，再用这把根证书校验服务端签名。
// 已入网的节点额外出示证书并对同一份摘要签名，证明自己握有对应私钥。
func (a *agent) handshakeClient(t handshakeTransport) (*secureChannel, error) {
	curve := ecdh.X25519()
	ephC, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	nonceA, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	ephCBytes := ephC.PublicKey().Bytes()

	hello := wsHello{
		T: "hello", V: wsProtoVersion, Mode: "resume",
		Nonce: b64(nonceA), Eph: b64(ephCBytes),
		Name: a.name, Version: version, OS: a.runtimeOS, Arch: a.runtimeArch, CPUs: a.runtimeCPUs,
	}
	if a.hasIdentity() {
		hello.NodeID = a.state.NodeID
		hello.Cert = string(a.certPEM)
	} else {
		hello.Mode = "enroll"
		hello.Token = a.cfg.Token
		hello.CSR = a.csr
	}
	if err := t.sendJSON(hello); err != nil {
		return nil, err
	}

	var challenge wsChallenge
	if err := t.recvJSON(&challenge); err != nil {
		return nil, fmt.Errorf("读取主控握手响应失败: %w", err)
	}
	if challenge.T == "error" {
		return nil, errors.New(challenge.Error)
	}
	if challenge.T != "challenge" {
		return nil, errors.New("握手顺序错误")
	}

	caCert, err := parseCertPEM([]byte(challenge.CA))
	if err != nil {
		return nil, fmt.Errorf("主控 CA 证书无法解析: %w", err)
	}
	want := a.fingerprint()
	if got := sha256Hex(caCert.Raw); got != want {
		return nil, fmt.Errorf("主控 CA 指纹不匹配：期望 %s，收到 %s",
			groupFingerprint(want), groupFingerprint(got))
	}
	nonceM, err := unb64(challenge.Nonce)
	if err != nil || len(nonceM) != 32 {
		return nil, errors.New("主控 nonce 不合法")
	}
	ephSBytes, err := unb64(challenge.Eph)
	if err != nil || len(ephSBytes) != 32 {
		return nil, errors.New("主控临时公钥不合法")
	}
	serverSig, err := unb64(challenge.Sig)
	if err != nil {
		return nil, errors.New("主控签名不合法")
	}
	serverDigest := wsTranscript(wsRoleServer, nonceA, nonceM, ephCBytes, ephSBytes, nil)
	if err := verifyDigest(caCert.PublicKey, serverDigest, serverSig); err != nil {
		return nil, fmt.Errorf("主控身份校验失败: %w", err)
	}

	auth := wsAuth{T: "auth"}
	if a.hasIdentity() {
		leaf, err := parseCertPEM(a.certPEM)
		if err != nil {
			return nil, err
		}
		clientDigest := wsTranscript(wsRoleClient, nonceA, nonceM, ephCBytes, ephSBytes, leaf.Raw)
		sig, err := signDigest(a.key, clientDigest)
		if err != nil {
			return nil, err
		}
		auth.Sig = b64(sig)
	}
	if err := t.sendJSON(auth); err != nil {
		return nil, err
	}

	peerPub, err := curve.NewPublicKey(ephSBytes)
	if err != nil {
		return nil, errors.New("主控临时公钥不合法")
	}
	shared, err := ephC.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("密钥协商失败: %w", err)
	}
	kC2S, kS2C, err := wsSessionKeys(shared, nonceA, nonceM)
	if err != nil {
		return nil, err
	}
	return newSecureChannel(kC2S, kS2C, false)
}

// decodeControl 解析一帧明文控制消息。
func decodeControl(plain []byte) (wsControl, error) {
	var msg wsControl
	if err := json.Unmarshal(plain, &msg); err != nil {
		return msg, err
	}
	if msg.T == "" {
		return msg, errors.New("控制帧缺少类型")
	}
	return msg, nil
}
