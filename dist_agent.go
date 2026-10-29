package main

import (
	"bytes"
	"context"
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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
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
// 这条约束让压测机可以放在最严格的网络分区里，也不需要为它开口子。
type agent struct {
	dir     string
	name    string
	state   agentState
	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
	boot    *http.Client // 首次入网：只有指纹，没有客户端证书
	api     *http.Client // 入网后：mTLS

	mu      sync.Mutex
	running string
	cancel  context.CancelFunc
	warned  map[string]string
}

func newAgent(cfg agentConfig) (*agent, error) {
	if cfg.Dir == "" {
		return nil, errors.New("缺少从节点数据目录")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", cfg.Dir, err)
	}
	a := &agent{dir: cfg.Dir, name: sanitizeNodeName(cfg.Name), warned: map[string]string{}}
	if a.name == "" {
		if hn, err := os.Hostname(); err == nil {
			a.name = sanitizeNodeName(hn)
		}
	}

	state, err := a.loadState()
	switch {
	case err == nil:
		if cfg.URL != "" && strings.TrimSuffix(cfg.URL, "/") != state.MasterURL {
			return nil, fmt.Errorf(
				"数据目录 %s 已加入 %s，不能改连 %s；换主控请换 -data 或清空该目录",
				cfg.Dir, state.MasterURL, strings.TrimSuffix(cfg.URL, "/"))
		}
		if fp := normalizeFingerprint(cfg.Fingerprint); fp != "" && fp != state.Fingerprint {
			return nil, fmt.Errorf("数据目录 %s 记录的 CA 指纹与本次不一致，拒绝连接", cfg.Dir)
		}
		a.state = state
		if err := a.loadMaterials(); err != nil {
			return nil, err
		}
		a.buildBootClient()
		if err := a.buildAPIClient(); err != nil {
			return nil, err
		}
		leaf, err := a.leafCert()
		if err != nil {
			return nil, err
		}
		if time.Now().After(leaf.NotAfter) {
			return nil, fmt.Errorf("节点证书已于 %s 过期，请在主控控制台重新签发加入命令并用新的 -data 目录加入",
				leaf.NotAfter.Format(time.RFC3339))
		}
		return a, nil
	case errors.Is(err, os.ErrNotExist):
		// 首次入网
		if err := a.enroll(cfg); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, err
	}
}

func (a *agent) statePath() string        { return filepath.Join(a.dir, "agent.json") }
func (a *agent) caPath() string           { return filepath.Join(a.dir, "ca.crt") }
func (a *agent) certPath() string         { return filepath.Join(a.dir, "agent.crt") }
func (a *agent) keyPath() string          { return filepath.Join(a.dir, "agent.key") }
func (a *agent) endpoint(p string) string { return a.state.MasterURL + p }

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
	key, err := os.ReadFile(a.keyPath())
	if err != nil {
		return fmt.Errorf("读取 %s: %w", a.keyPath(), err)
	}
	a.caPEM, a.certPEM, a.keyPEM = ca, cert, key
	return nil
}

// buildBootClient 构造首次入网用的客户端。
//
// 这一步刻意不需要 CA 证书：节点手里当时只有主控打印的一枚指纹，
// 所以校验方式是"链根的 SHA-256 等于这枚指纹，且叶子确实由该根签发"。
func (a *agent) buildBootClient() {
	a.boot = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				ServerName:         distServerName,
				InsecureSkipVerify: true,
				VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					return verifyPinnedChain(raw, a.state.Fingerprint)
				},
			},
			ForceAttemptHTTP2: true,
		},
		Timeout: 45 * time.Second,
	}
}

// buildAPIClient 构造入网后使用的 mTLS 客户端。
func (a *agent) buildAPIClient() error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(a.caPEM) {
		return errors.New("本地 CA 证书无法解析，请清空数据目录重新加入")
	}
	pair, err := tls.X509KeyPair(a.certPEM, a.keyPEM)
	if err != nil {
		return fmt.Errorf("节点证书与私钥不匹配: %w", err)
	}
	api := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      roots,
				ServerName:   distServerName,
				Certificates: []tls.Certificate{pair},
				MinVersion:   tls.VersionTLS13,
			},
			ForceAttemptHTTP2: true,
		},
		Timeout: 45 * time.Second,
	}
	// 证书换发会重建这个客户端，而进度上报跑在任务 goroutine 里，
	// 所以整体替换必须走锁，不能直接赋值。
	a.mu.Lock()
	a.api = api
	a.mu.Unlock()
	return nil
}

// enroll 用一次性令牌换一张客户端证书。
func (a *agent) enroll(cfg agentConfig) error {
	rawURL := strings.TrimSuffix(strings.TrimSpace(cfg.URL), "/")
	if rawURL == "" {
		return errors.New("缺少主控地址，请用 -join 或 -master-url 指定")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("主控地址必须是 https://host:port 形式，收到 %q", rawURL)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return errors.New("缺少加入令牌，请在主控控制台点「加入命令」获取完整的 -join 值")
	}
	fp := normalizeFingerprint(cfg.Fingerprint)
	if fp == "" && cfg.CAFile != "" {
		pemData, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return fmt.Errorf("读取 CA 证书 %s: %w", cfg.CAFile, err)
		}
		cert, err := parseCertPEM(pemData)
		if err != nil {
			return fmt.Errorf("解析 CA 证书 %s: %w", cfg.CAFile, err)
		}
		fp = sha256Hex(cert.Raw)
	}
	if len(fp) != 64 {
		return errors.New("缺少可用的 CA 指纹：请用 -join 或 -ca-fingerprint / -ca-cert 指定")
	}

	a.state = agentState{MasterURL: rawURL, Fingerprint: fp, Name: a.name}
	a.buildBootClient()

	key, csrPEM, err := newAgentCSR(a.name)
	if err != nil {
		return err
	}
	req := enrollRequest{
		Token:   cfg.Token,
		CSR:     csrPEM,
		Name:    a.name,
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		CPUs:    runtime.NumCPU(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp enrollResponse
	if err := a.postJSON(ctx, a.boot, a.endpoint("/api/v1/enroll"), req, &resp); err != nil {
		return fmt.Errorf("入网失败: %w", err)
	}
	if resp.NodeID == "" || resp.CertPEM == "" {
		return errors.New("主控返回的入网响应不完整")
	}

	// 校验主控签回来的证书：身份必须是它分配的节点 ID，
	// 公钥必须对应我们刚生成的私钥。少了这一步，一个错误的主控地址
	// 也能让节点拿着一把对不上号的证书"看起来加入了"。
	leaf, err := parseCertPEM([]byte(resp.CertPEM))
	if err != nil {
		return fmt.Errorf("解析节点证书: %w", err)
	}
	if leaf.Subject.CommonName != resp.NodeID {
		return fmt.Errorf("证书身份 %q 与分配的节点 ID %q 不一致", leaf.Subject.CommonName, resp.NodeID)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("主控返回的证书与本地私钥不匹配")
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return err
	}

	a.state.NodeID = resp.NodeID
	a.state.EnrolledAt = time.Now()
	a.caPEM = []byte(resp.CAPEM)
	a.certPEM = []byte(resp.CertPEM)
	a.keyPEM = keyPEM
	if err := a.saveState(); err != nil {
		return err
	}
	if err := a.buildAPIClient(); err != nil {
		return err
	}
	distLogf("agent: 已加入 %s，节点 ID %s（%s）", a.state.MasterURL, a.state.NodeID, a.name)
	return nil
}

func (a *agent) saveState() error {
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.caPath(), a.caPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(a.certPath(), a.certPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(a.keyPath(), a.keyPEM, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(a.statePath(), data, 0o600)
}

func (a *agent) leafCert() (*x509.Certificate, error) {
	return parseCertPEM(a.certPEM)
}

// run 是常驻循环：拉命令 → 执行 → 续期，主控不可达时指数退避。
func (a *agent) run(ctx context.Context) error {
	distLogf("agent: %s 就绪，主控 %s", a.state.NodeID, a.state.MasterURL)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		resp, err := a.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			distLogf("agent: 与主控通信失败：%v（%s 后重试）", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if resp.Command != nil {
			a.handleCommand(ctx, *resp.Command)
		}
		if err := a.renewIfDue(ctx); err != nil {
			a.warn("renew", err)
		}
	}
}

func (a *agent) poll(ctx context.Context) (pollResponse, error) {
	req := pollRequest{
		Proto:   distProtoVersion,
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		CPUs:    runtime.NumCPU(),
		Running: a.currentTask(),
	}
	// 客户端超时要比服务端的长轮询窗口更长，否则每次空闲等待都会变成一次失败重连。
	ctx2, cancel := context.WithTimeout(ctx, distPollWait+20*time.Second)
	defer cancel()
	var resp pollResponse
	if err := a.postJSON(ctx2, a.client(), a.endpoint("/api/v1/agent/poll"), req, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func (a *agent) handleCommand(ctx context.Context, cmd Command) {
	switch cmd.Kind {
	case cmdStart:
		if cmd.Spec == nil {
			distLogf("agent: 收到没有任务内容的 start 命令，已忽略")
			return
		}
		if cur := a.currentTask(); cur != "" {
			distLogf("agent: 正在执行 %s，忽略新任务 %s", cur, cmd.TaskID)
			return
		}
		a.startTask(ctx, *cmd.Spec)
	case cmdStop:
		a.mu.Lock()
		cancel, running := a.cancel, a.running
		a.mu.Unlock()
		if running == cmd.TaskID && cancel != nil {
			cancel()
		}
	}
}

func (a *agent) startTask(parent context.Context, spec TaskSpec) {
	taskCtx, cancel := context.WithCancel(parent)
	a.mu.Lock()
	a.running = spec.ID
	a.cancel = cancel
	a.mu.Unlock()

	distLogf("agent: 开始任务 %s：%d 并发 × %d 秒 -> %s",
		spec.ID, spec.PerNodeConc, spec.DurationSec, spec.Target)

	go func() {
		defer cancel()
		var seq uint64
		res := runDistTask(taskCtx, &spec, spec.PerNodeConc, func(p Progress) (bool, bool) {
			seq++
			p.TaskID = spec.ID
			p.Seq = seq
			stop, byDeadline, err := a.reportProgress(p)
			if err != nil {
				a.warn("progress", err)
			}
			return stop, byDeadline
		})
		res.TaskID = spec.ID
		if err := a.reportResult(res); err != nil {
			a.warn("result", err)
		}
		a.mu.Lock()
		if a.running == spec.ID {
			a.running, a.cancel = "", nil
		}
		a.mu.Unlock()
		distLogf("agent: 任务 %s 结束（%s）：%d 请求 / %d 失败 / %s / %.1f 秒",
			spec.ID, res.Status, res.Requests, res.Failed,
			readableBytes(float64(res.Bytes)), float64(res.ElapsedMs)/1000)
	}()
}

func (a *agent) currentTask() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// client 返回当前生效的 mTLS 客户端。
// 换发证书时会整体替换它，因此调用方不能直接读字段。
func (a *agent) client() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.api
}

// reportProgress 用独立上下文上报：任务被打断时请求上下文已经取消，
// 但最后一帧进度和最终结果仍然必须发出去，否则主控只能把它判成 lost。
func (a *agent) reportProgress(p Progress) (stop, byDeadline bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var out struct {
		Stop   bool   `json:"stop"`
		Reason string `json:"reason"`
	}
	if err := a.postJSON(ctx, a.client(), a.endpoint("/api/v1/agent/progress"), p, &out); err != nil {
		return false, false, err
	}
	return out.Stop, out.Reason == "deadline", nil
}

func (a *agent) reportResult(res TaskResult) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := a.postJSON(ctx, a.client(), a.endpoint("/api/v1/agent/result"), res, nil)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return lastErr
}

// renewIfDue 在证书进入最后 30 天时换发。
// 换发成功后主控立刻把登记的序列号改成新的，旧证书随即失效，
// 所以这是一次"原子替换"，不存在新旧并存的窗口。
func (a *agent) renewIfDue(ctx context.Context) error {
	leaf, err := a.leafCert()
	if err != nil {
		return err
	}
	if time.Until(leaf.NotAfter) > distRenewBefore {
		return nil
	}
	key, csrPEM, err := newAgentCSR(a.name)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var resp renewResponse
	if err := a.postJSON(reqCtx, a.client(), a.endpoint("/api/v1/agent/renew"), renewRequest{CSR: csrPEM}, &resp); err != nil {
		return err
	}
	newLeaf, err := parseCertPEM([]byte(resp.CertPEM))
	if err != nil {
		return err
	}
	if newLeaf.Subject.CommonName != a.state.NodeID {
		return errors.New("换发证书的身份与当前节点不一致")
	}
	pub, ok := newLeaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("换发证书与本地新私钥不匹配")
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return err
	}
	// 主控已经把登记序列号换掉了，内存里必须立刻切到新证书，
	// 否则下一次请求就会因为"证书已失效"被拒。
	a.certPEM, a.keyPEM = []byte(resp.CertPEM), keyPEM
	if err := a.buildAPIClient(); err != nil {
		return err
	}
	if err := a.saveState(); err != nil {
		distLogf("agent: 证书已换发但落盘失败：%v", err)
	}
	distLogf("agent: 证书已换发，新有效期至 %s", newLeaf.NotAfter.Format("2006-01-02"))
	return nil
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

func (a *agent) postJSON(ctx context.Context, client *http.Client, target string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, distMaxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("主控返回 %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// newAgentCSR 生成新的 P-256 私钥与对应 CSR。
// 主控只接受 P-256/P-384 的 ECDSA 或 2048 位以上的 RSA，
// 这里固定用 P-256：握手快、体积小，安全性足够。
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
