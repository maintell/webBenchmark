package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// agentConfig 描述从节点如何加入主控。
type agentConfig struct {
	Dir         string
	URL         string
	Name        string
	Token       string
	Fingerprint string
	CAFile      string
}

// agentState 是入网后落盘的持久身份，重启后靠它免去重新申请。
type agentState struct {
	NodeID      string    `json:"nodeId"`
	Name        string    `json:"name"`
	MasterURL   string    `json:"masterUrl"`
	Fingerprint string    `json:"fingerprint"`
	EnrolledAt  time.Time `json:"enrolledAt"`
}

// agent 是从节点：只向外连接主控，本机不监听任何端口。
// 这条约束让压测机可以待在最严格的网络分区里，也不必为它开任何入站规则。
type agent struct {
	dir   string
	name  string
	cfg   agentConfig
	state agentState

	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
	key     crypto.Signer
	csr     string

	runtimeOS   string
	runtimeArch string
	runtimeCPUs int

	mu       sync.Mutex
	running  string
	stopCh   chan string
	cancel   context.CancelFunc
	warned   map[string]string
	pending  []TaskResult
	renewKey crypto.Signer
	renewPEM []byte
}

func newAgent(cfg agentConfig) (*agent, error) {
	if cfg.Dir == "" {
		return nil, errors.New("缺少从节点数据目录")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", cfg.Dir, err)
	}
	a := &agent{
		dir:         cfg.Dir,
		cfg:         cfg,
		name:        sanitizeNodeName(cfg.Name),
		warned:      map[string]string{},
		runtimeOS:   runtime.GOOS,
		runtimeArch: runtime.GOARCH,
		runtimeCPUs: runtime.NumCPU(),
	}
	if a.name == "" {
		if hn, err := os.Hostname(); err == nil {
			a.name = sanitizeNodeName(hn)
		}
	}

	state, err := a.loadState()
	switch {
	case err == nil:
		a.state = state
		if cfg.URL != "" && strings.TrimSuffix(cfg.URL, "/") != state.MasterURL {
			return nil, fmt.Errorf(
				"数据目录 %s 已加入 %s，不能改连 %s；换主控请换 -data 或清空该目录",
				cfg.Dir, state.MasterURL, strings.TrimSuffix(cfg.URL, "/"))
		}
		if fp := normalizeFingerprint(cfg.Fingerprint); fp != "" && fp != state.Fingerprint {
			return nil, fmt.Errorf("数据目录 %s 记录的 CA 指纹与本次不一致，拒绝连接", cfg.Dir)
		}
		if err := a.loadMaterials(); err != nil {
			return nil, err
		}
		leaf, err := parseCertPEM(a.certPEM)
		if err != nil {
			return nil, err
		}
		if time.Now().After(leaf.NotAfter) {
			return nil, fmt.Errorf("节点证书已于 %s 过期，请在主控控制台重新签发加入命令，并用新的 -data 目录加入",
				leaf.NotAfter.Format(time.RFC3339))
		}
		return a, nil
	case errors.Is(err, os.ErrNotExist):
		if err := a.prepareEnrollment(); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, err
	}
}

func (a *agent) statePath() string { return filepath.Join(a.dir, "agent.json") }
func (a *agent) caPath() string    { return filepath.Join(a.dir, "ca.crt") }
func (a *agent) certPath() string  { return filepath.Join(a.dir, "agent.crt") }
func (a *agent) keyPath() string   { return filepath.Join(a.dir, "agent.key") }

func (a *agent) hasIdentity() bool {
	return len(a.certPEM) > 0 && a.key != nil
}

// prepareEnrollment 校验"首次入网"需要的输入并生成密钥与 CSR。
// 真正的入网在 WebSocket 握手时完成，这里不做任何网络请求。
func (a *agent) prepareEnrollment() error {
	rawURL := strings.TrimSuffix(strings.TrimSpace(a.cfg.URL), "/")
	if rawURL == "" {
		return errors.New("缺少主控地址，请用 -join 或 -master-url 指定")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("主控地址必须是 http:// 或 https:// 开头，收到 %q", rawURL)
	}
	if strings.TrimSpace(a.cfg.Token) == "" {
		return errors.New("缺少加入令牌，请在主控控制台点「加入命令」获取完整的 -join 值")
	}
	fp := normalizeFingerprint(a.cfg.Fingerprint)
	if fp == "" && a.cfg.CAFile != "" {
		pemData, err := os.ReadFile(a.cfg.CAFile)
		if err != nil {
			return fmt.Errorf("读取 CA 证书 %s: %w", a.cfg.CAFile, err)
		}
		cert, err := parseCertPEM(pemData)
		if err != nil {
			return fmt.Errorf("解析 CA 证书 %s: %w", a.cfg.CAFile, err)
		}
		fp = sha256Hex(cert.Raw)
		if a.cfg.CAFile != "" {
			a.caPEM = pemData
		}
	}
	if len(fp) != 64 {
		return errors.New("缺少可用的 CA 指纹：请用 -join，或同时给出 -ca-fingerprint / -ca-cert")
	}
	key, csr, err := newAgentCSR(a.name)
	if err != nil {
		return err
	}
	a.key, a.csr = key, csr
	a.state = agentState{MasterURL: rawURL, Fingerprint: fp, Name: a.name}
	a.cfg.Token = strings.TrimSpace(a.cfg.Token)
	return nil
}

func (a *agent) loadState() (agentState, error) {
	var st agentState
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("解析 %s: %w", a.statePath(), err)
	}
	if st.NodeID == "" || st.MasterURL == "" {
		return st, fmt.Errorf("%s 内容不完整", a.statePath())
	}
	return st, nil
}

func (a *agent) loadMaterials() error {
	ca, err := os.ReadFile(a.caPath())
	if err != nil {
		return fmt.Errorf("读取 %s: %w", a.caPath(), err)
	}
	cert, err := os.ReadFile(a.certPath())
	if err != nil {
		return fmt.Errorf("读取 %s: %w", a.certPath(), err)
	}
	keyPEM, err := os.ReadFile(a.keyPath())
	if err != nil {
		return fmt.Errorf("读取 %s: %w", a.keyPath(), err)
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("解析 %s: %w", a.keyPath(), err)
	}
	a.caPEM, a.certPEM, a.keyPEM, a.key = ca, cert, keyPEM, key
	return nil
}

func (a *agent) saveState() error {
	if err := writeFileAtomic(a.caPath(), a.caPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(a.certPath(), a.certPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(a.keyPath(), a.keyPEM, 0o600); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.statePath(), data, 0o600)
}

// wsURL 把主控地址翻译成通道地址：https -> wss，http -> ws。
func (a *agent) wsURL() (string, error) {
	u, err := url.Parse(a.state.MasterURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("主控地址不合法: %q", a.state.MasterURL)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("主控地址只支持 http / https，收到 %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/agent/ws"
	return u.String(), nil
}

// fingerprint 返回本节点信任的 CA 指纹：首次入网来自凭据，重启后来自本地状态。
func (a *agent) fingerprint() string {
	if fp := normalizeFingerprint(a.cfg.Fingerprint); len(fp) == 64 {
		return fp
	}
	return normalizeFingerprint(a.state.Fingerprint)
}

// tlsConfig 只在 wss 下使用。
//
// 两层校验：先按"本集群 CA"固定校验（自签部署），失败再退回系统根 + 主机名
// （主控挂在 nginx/caddy 后面用公网证书的部署）。无论哪一层通过，真正的身份
// 都由应用层握手确认，外层 TLS 只是额外的传输保护。
func (a *agent) tlsConfig() *tls.Config {
	u, err := url.Parse(a.state.MasterURL)
	if err != nil || u.Scheme != "https" {
		return nil
	}
	fp := a.fingerprint()
	host := u.Hostname()
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			return verifyMasterTLSCert(raw, fp, host)
		},
	}
}

func verifyMasterTLSCert(rawCerts [][]byte, fingerprint, host string) error {
	if len(rawCerts) == 0 {
		return errors.New("对端没有出示证书")
	}
	if len(normalizeFingerprint(fingerprint)) == 64 {
		if err := verifyPinnedChain(rawCerts, fingerprint); err == nil {
			return nil
		}
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("解析对端证书: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
		return errors.New("主控 TLS 证书既不是本集群 CA 签发，也不受系统信任")
	}
	return nil
}

// agentSession 是一条已建立的主从连接。
// 写端加锁：进度、结果、心跳、pong 都可能来自不同协程，而帧序号必须严格有序。
type agentSession struct {
	agent *agent
	ws    *wsConn
	ch    *secureChannel
	mu    sync.Mutex
}

func (s *agentSession) Send(msg wsControl) error {
	plain, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws.sendBinary(s.ch.seal(plain))
}

func (s *agentSession) Recv() (wsControl, error) {
	kind, data, err := s.ws.readMessage()
	if err != nil {
		return wsControl{}, err
	}
	if kind != websocket.BinaryMessage {
		return wsControl{}, errors.New("收到非预期的文本帧")
	}
	plain, err := s.ch.open(data)
	if err != nil {
		return wsControl{}, err
	}
	return decodeControl(plain)
}

func (s *agentSession) Close() {
	s.ws.close()
}

// connect 建立会话（首次会入网），返回可直接收发控制帧的会话。
func (a *agent) connect(ctx context.Context) (*agentSession, error) {
	wsURL, err := a.wsURL()
	if err != nil {
		return nil, err
	}
	conn, err := wsDial(ctx, wsURL, a.tlsConfig())
	if err != nil {
		return nil, err
	}
	ch, err := a.handshakeClient(conn)
	if err != nil {
		conn.close()
		return nil, err
	}
	sess := &agentSession{agent: a, ws: conn, ch: ch}

	ready, err := sess.Recv()
	if err != nil {
		conn.close()
		return nil, fmt.Errorf("主控没有确认会话: %w", err)
	}
	if ready.T != "ready" {
		conn.close()
		return nil, fmt.Errorf("主控返回了意外消息 %q", ready.T)
	}
	if err := a.applyReady(ready); err != nil {
		conn.close()
		return nil, err
	}
	return sess, nil
}

// applyReady 处理主控的第一条加密帧：入网成功时它会带回节点证书。
func (a *agent) applyReady(ready wsControl) error {
	if ready.NodeID == "" {
		return errors.New("主控没有分配节点 ID")
	}
	if ready.Cert == "" {
		if a.state.NodeID != "" && a.state.NodeID != ready.NodeID {
			return fmt.Errorf("主控确认的身份 %s 与本地记录 %s 不一致", ready.NodeID, a.state.NodeID)
		}
		a.state.NodeID = ready.NodeID
		return nil
	}

	leaf, err := parseCertPEM([]byte(ready.Cert))
	if err != nil {
		return fmt.Errorf("解析节点证书: %w", err)
	}
	if leaf.Subject.CommonName != ready.NodeID {
		return fmt.Errorf("证书身份 %q 与分配的节点 ID %q 不一致", leaf.Subject.CommonName, ready.NodeID)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || a.key == nil || !pub.Equal(a.key.Public()) {
		return errors.New("主控返回的证书与本地私钥不匹配")
	}
	keyPEM, err := encodeKeyPEM(a.key)
	if err != nil {
		return err
	}
	a.certPEM = []byte(ready.Cert)
	a.keyPEM = keyPEM
	a.state.NodeID = ready.NodeID
	a.state.EnrolledAt = time.Now()
	a.csr = "" // 私钥已经落盘，CSR 不再需要
	if err := a.saveState(); err != nil {
		return err
	}
	distLogf("agent: 已加入 %s，节点 ID %s（%s）", a.state.MasterURL, a.state.NodeID, a.name)
	return nil
}

// run 是常驻循环：连上就服务，断了就指数退避重连。
func (a *agent) run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.serveSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			distLogf("agent: 与主控的连接中断：%v（%s 后重连）", err, backoff)
		}
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *agent) serveSession(ctx context.Context) error {
	sess, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer sess.Close()
	distLogf("agent: 已连接 %s（%s）", a.state.MasterURL, a.state.NodeID)

	// 连接断开不应打断正在跑的任务：它按自己的时长跑完，
	// 结果进入待发队列，重连后自动补报。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()

	a.sendHeartbeat(sess)
	a.maybeRenew(sess)
	a.flushPending(sess)

	hb := time.NewTicker(10 * time.Second)
	defer hb.Stop()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-hb.C:
				a.sendHeartbeat(sess)
			}
		}
	}()

	for {
		msg, err := sess.Recv()
		if err != nil {
			return err
		}
		switch msg.T {
		case "start":
			a.startTask(ctx, sess, msg)
		case "stop":
			a.stopRunning(msg.TaskID, msg.Reason)
		case "renewed":
			a.applyRenewal(msg)
		case "ping":
			_ = sess.Send(wsControl{T: "pong"})
		}
	}
}

func (a *agent) sendHeartbeat(sess *agentSession) {
	hb := &wsHeartbeat{
		Running: a.currentTask(),
		Version: version,
		OS:      a.runtimeOS,
		Arch:    a.runtimeArch,
		CPUs:    a.runtimeCPUs,
	}
	if err := sess.Send(wsControl{T: "heartbeat", HB: hb}); err != nil {
		a.warn("heartbeat", err)
	}
}

func (a *agent) currentTask() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *agent) startTask(parent context.Context, sess *agentSession, msg wsControl) {
	spec := msg.Spec
	if spec == nil {
		distLogf("agent: 收到没有任务内容的 start 命令，已忽略")
		return
	}
	if cur := a.currentTask(); cur != "" {
		distLogf("agent: 正在执行 %s，忽略新任务 %s", cur, spec.ID)
		return
	}

	taskCtx, cancel := context.WithCancel(parent)
	stopCh := make(chan string, 1)
	a.mu.Lock()
	a.running = spec.ID
	a.stopCh = stopCh
	a.cancel = cancel
	a.mu.Unlock()

	distLogf("agent: 开始任务 %s：%d 并发 × %d 秒 -> %s",
		spec.ID, spec.PerNodeConc, spec.DurationSec, spec.Target)

	go func() {
		defer cancel()
		var seq uint64
		res := runDistTask(taskCtx, spec, spec.PerNodeConc, func(p Progress) {
			seq++
			p.TaskID = spec.ID
			p.Seq = seq
			if err := sess.Send(wsControl{T: "progress", P: &p}); err != nil {
				a.warn("progress", err)
			}
		}, stopCh)
		res.TaskID = spec.ID
		a.deliverResult(sess, res)

		a.mu.Lock()
		if a.running == spec.ID {
			a.running, a.stopCh, a.cancel = "", nil, nil
		}
		a.mu.Unlock()
		distLogf("agent: 任务 %s 结束（%s）：%d 请求 / %d 失败 / %s / %.1f 秒",
			spec.ID, res.Status, res.Requests, res.Failed,
			readableBytes(float64(res.Bytes)), float64(res.ElapsedMs)/1000)
	}()
}

// deliverResult 先尝试直接上报；失败就留一份到待发队列，等重连补报。
// 压测结果来之不易，不能因为一次网络抖动就丢掉。
func (a *agent) deliverResult(sess *agentSession, res TaskResult) {
	if err := sess.Send(wsControl{T: "result", R: &res}); err == nil {
		return
	}
	a.mu.Lock()
	a.pending = append(a.pending, res)
	if len(a.pending) > 8 {
		a.pending = a.pending[len(a.pending)-8:]
	}
	a.mu.Unlock()
}

func (a *agent) flushPending(sess *agentSession) {
	a.mu.Lock()
	queued := a.pending
	a.pending = nil
	a.mu.Unlock()
	for i := range queued {
		res := queued[i]
		if err := sess.Send(wsControl{T: "result", R: &res}); err != nil {
			a.mu.Lock()
			a.pending = append(a.pending, res)
			a.mu.Unlock()
			return
		}
	}
}

func (a *agent) stopRunning(taskID, reason string) {
	a.mu.Lock()
	stopCh, running := a.stopCh, a.running
	a.mu.Unlock()
	if running == "" || running != taskID || stopCh == nil {
		return
	}
	if reason == "" {
		reason = "stopped"
	}
	select {
	case stopCh <- reason:
	default:
	}
}

// maybeRenew 在证书进入最后 30 天时申请换发。
func (a *agent) maybeRenew(sess *agentSession) {
	leaf, err := parseCertPEM(a.certPEM)
	if err != nil {
		return
	}
	if time.Until(leaf.NotAfter) > distRenewBefore {
		return
	}
	key, csr, err := newAgentCSR(a.name)
	if err != nil {
		return
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.renewKey, a.renewPEM = key, keyPEM
	a.mu.Unlock()
	if err := sess.Send(wsControl{T: "renew", CSR: csr}); err != nil {
		a.warn("renew", err)
	}
}

// applyRenewal 换上新证书。主控已经把登记序列号改成新的，
// 所以本地必须立刻切换，否则下一次握手就会被判成"证书已失效"。
func (a *agent) applyRenewal(msg wsControl) {
	if msg.Error != "" {
		a.warn("renew", errors.New(msg.Error))
		return
	}
	a.mu.Lock()
	key, keyPEM := a.renewKey, a.renewPEM
	a.renewKey, a.renewPEM = nil, nil
	a.mu.Unlock()
	if key == nil || msg.Cert == "" {
		return
	}
	leaf, err := parseCertPEM([]byte(msg.Cert))
	if err != nil {
		a.warn("renew", err)
		return
	}
	if leaf.Subject.CommonName != a.state.NodeID {
		a.warn("renew", errors.New("换发证书的身份与当前节点不一致"))
		return
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		a.warn("renew", errors.New("换发证书与本地新私钥不匹配"))
		return
	}
	a.certPEM, a.keyPEM, a.key = []byte(msg.Cert), keyPEM, key
	if err := a.saveState(); err != nil {
		distLogf("agent: 证书已换发但落盘失败：%v", err)
	}
	distLogf("agent: 证书已换发，新有效期至 %s", leaf.NotAfter.Format("2006-01-02"))
}

// warn 对同一类错误只打印一次变化，避免离线时把日志刷满。
func (a *agent) warn(key string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	a.mu.Lock()
	prev, seen := a.warned[key]
	a.warned[key] = msg
	a.mu.Unlock()
	if !seen || prev != msg {
		distLogf("agent: %s 失败：%s", key, shortText(msg, 200))
	}
}

// newAgentCSR 生成新的 P-256 私钥与对应 CSR。
// 主控只接受 P-256/P-384 的 ECDSA 或 2048 位以上的 RSA；这里固定 P-256，
// 握手快、体积小，安全性足够。
func newAgentCSR(name string) (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	subj := pkix.Name{CommonName: "webBenchmark agent"}
	if name != "" {
		subj.Organization = []string{name}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subj}, key)
	if err != nil {
		return nil, "", err
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func distLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
