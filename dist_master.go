package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	distMaxBodyBytes = 64 << 10
	// 停止后留给节点上报最终结果的时间。超过这个窗口仍无消息的节点
	// 会被标记成 lost，任务才能收尾，而不是永远停在 running。
	distStopGrace = 10 * time.Second
	// 运行中连续这么久没有任何进度，就认为节点已经掉线（agent 每秒上报一次）。
	distLostAfter = 25 * time.Second
	distSeriesMax = 600
	distMaxNodes  = 512
	distKeptTasks = 20
)

// masterOptions 把主控的可调项收成一个结构，测试里可以只要一个临时目录。
type masterOptions struct {
	Dir       string
	Hosts     []string
	JoinTTL   time.Duration
	AdminHash []byte
	MaxNodes  int
	// PollWait 是从节点长轮询的等待窗口。留空用 distPollWait；
	// 测试把它调短，否则每个"没有命令"的断言都要白等 20 秒。
	PollWait time.Duration
}

// master 是主控节点：持有信任根、节点注册表、任务运行态和控制台 HTTP 服务。
//
// 锁的约定：mu 保护 nodes / tasks / order / history / usedTokens。
// 所有会阻塞的 I/O（写状态文件）都在持锁状态下完成，因为它们体积很小（几百字节），
// 而脱离锁去写会引入"状态文件与内存不一致"这类难查的问题。
type master struct {
	dir        string
	pki        *pki
	serverCert tls.Certificate
	tlsConfig  *tls.Config
	adminHash  []byte
	joinTTL    time.Duration
	maxNodes   int
	pollWait   time.Duration
	sessions   *sessionStore
	login      *rateLimiter
	enroll     *rateLimiter
	audit      *auditLog
	started    time.Time

	mu         sync.Mutex
	nodes      map[string]*nodeRuntime
	tasks      map[string]*taskRuntime
	order      []string
	history    []taskView
	usedTokens map[string]int64
}

// nodeRuntime 是节点在主控内存里的全部状态。
// 持久化的只有身份字段，lastSeen / pending 这类在线态重启后归零是正确语义。
type nodeRuntime struct {
	id         string
	name       string
	serial     string
	enrolledAt time.Time
	revoked    bool
	revokedAt  time.Time

	lastSeen   time.Time
	version    string
	os         string
	arch       string
	cpus       int
	remoteAddr string
	running    string
	pending    []Command
	notify     chan struct{}
}

type shardRuntime struct {
	nodeID    string
	conc      int
	state     string
	requests  uint64
	failed    uint64
	bytes     uint64
	seq       uint64
	rps       float64
	bps       float64
	lastError string
	lastAt    time.Time
	prevAt    time.Time
	prevReq   uint64
	prevBytes uint64
	stopSent  bool
}

type taskRuntime struct {
	spec      TaskSpec
	state     string
	started   time.Time
	endsAt    time.Time
	stoppedBy string
	shards    map[string]*shardRuntime
	series    []seriesPoint
	peakRPS   float64
	lastPoint time.Time
	done      chan struct{}
	finalized bool
}

type seriesPoint struct {
	T   int64   `json:"t"`
	RPS float64 `json:"rps"`
	BPS float64 `json:"bps"`
}

type taskTotals struct {
	Requests uint64  `json:"requests"`
	Failed   uint64  `json:"failed"`
	Bytes    uint64  `json:"bytes"`
	RPS      float64 `json:"rps"`
	BPS      float64 `json:"bps"`
	PeakRPS  float64 `json:"peakRps"`
}

type shardView struct {
	NodeID         string  `json:"nodeId"`
	Name           string  `json:"name"`
	State          string  `json:"state"`
	Conc           int     `json:"conc"`
	RPS            float64 `json:"rps"`
	BPS            float64 `json:"bps"`
	Requests       uint64  `json:"requests"`
	Failed         uint64  `json:"failed"`
	Bytes          uint64  `json:"bytes"`
	LastError      string  `json:"lastError,omitempty"`
	LastSeenAgoSec int     `json:"lastSeenAgoSec"`
}

// taskView 是控制台看到的任务全貌，同时也是落盘的格式：
// 一份结构同时承担"实时视图"和"历史记录"，避免两套模型漂移。
type taskView struct {
	ID          string        `json:"id"`
	Target      string        `json:"target"`
	Proto       string        `json:"proto"`
	DurationSec int           `json:"durationSec"`
	TotalConc   int           `json:"totalConc"`
	PerNodeConc int           `json:"perNodeConc"`
	NoReuse     bool          `json:"noReuse"`
	ProxySpoof  bool          `json:"proxySpoof"`
	XForwardFor bool          `json:"xforwardFor"`
	RandomPost  bool          `json:"randomPost"`
	RandomSize  int           `json:"randomSize"`
	Headers     []HeaderPair  `json:"headers,omitempty"`
	Family      string        `json:"family"`
	TimeoutSec  int           `json:"timeoutSec"`
	Note        string        `json:"note,omitempty"`
	State       string        `json:"state"`
	StartedAt   time.Time     `json:"startedAt"`
	EndsAt      time.Time     `json:"endsAt"`
	ElapsedMs   int64         `json:"elapsedMs"`
	RemainingMs int64         `json:"remainingMs"`
	StoppedBy   string        `json:"stoppedBy,omitempty"`
	Totals      taskTotals    `json:"totals"`
	Nodes       []shardView   `json:"nodes"`
	Series      []seriesPoint `json:"series"`
}

type nodeView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Online         bool      `json:"online"`
	Revoked        bool      `json:"revoked"`
	Version        string    `json:"version"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	CPUs           int       `json:"cpus"`
	LastSeenAgoSec int       `json:"lastSeenAgoSec"`
	Running        string    `json:"running,omitempty"`
	RPS            float64   `json:"rps"`
	BPS            float64   `json:"bps"`
	EnrolledAt     time.Time `json:"enrolledAt"`
}

// newMaster 装载（或首次创建）主控状态。
func newMaster(opts masterOptions) (*master, error) {
	if opts.JoinTTL <= 0 {
		opts.JoinTTL = 30 * time.Minute
	}
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = distMaxNodes
	}
	if opts.PollWait <= 0 {
		opts.PollWait = distPollWait
	}
	p, err := newPKI(opts.Dir)
	if err != nil {
		return nil, err
	}
	cert, err := p.issueServerCert(opts.Hosts)
	if err != nil {
		return nil, err
	}
	pool := p.caPool()
	audit, err := newAuditLog(filepath.Join(opts.Dir, "audit.log"))
	if err != nil {
		return nil, err
	}
	m := &master{
		dir:        opts.Dir,
		pki:        p,
		serverCert: cert,
		adminHash:  opts.AdminHash,
		joinTTL:    opts.JoinTTL,
		maxNodes:   opts.MaxNodes,
		pollWait:   opts.PollWait,
		sessions:   newSessionStore(distSessionTTL),
		login:      newRateLimiter(20, 5*time.Minute),
		enroll:     newRateLimiter(10, time.Minute),
		audit:      audit,
		started:    time.Now(),
		nodes:      make(map[string]*nodeRuntime),
		tasks:      make(map[string]*taskRuntime),
		usedTokens: make(map[string]int64),
		tlsConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// 只上 TLS 1.3：这套控制面只服务本工具自己的从节点和现代浏览器，
			// 没有任何理由为了兼容旧客户端把协议降到 1.2。
			MinVersion: tls.VersionTLS13,
			// 浏览器不带客户端证书，从节点必须带。带了的必须验通过，
			// 否则握手就失败——不会出现"证书坏了但请求照进"的中间态。
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  pool,
			NextProtos: []string{"h2", "http/1.1"},
		},
	}
	if err := m.loadState(); err != nil {
		audit.Close()
		return nil, err
	}
	m.loadHistory()
	return m, nil
}

func (m *master) close() {
	if m.audit != nil {
		m.audit.Close()
	}
}

// publicURL 给出加入命令里默认填写的主控地址。
// 监听在 0.0.0.0 时无法知道运维会从哪个地址访问，用主机名做最佳猜测，
// 控制台里允许改成实际可达的地址。
func masterPublicURL(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return "https://" + bind
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		if hn, err := os.Hostname(); err == nil && hn != "" {
			host = hn
		} else {
			host = "127.0.0.1"
		}
	}
	return "https://" + net.JoinHostPort(host, port)
}

// ---------- 状态持久化 ----------

type persistedNode struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Serial     string    `json:"serial"`
	EnrolledAt time.Time `json:"enrolledAt"`
	Revoked    bool      `json:"revoked"`
	RevokedAt  time.Time `json:"revokedAt"`
}

type persistedState struct {
	Nodes      []persistedNode  `json:"nodes"`
	UsedTokens map[string]int64 `json:"usedTokens"`
}

func (m *master) statePath() string { return filepath.Join(m.dir, "state.json") }

func (m *master) loadState() error {
	data, err := os.ReadFile(m.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s: %w", m.statePath(), err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("解析 %s: %w", m.statePath(), err)
	}
	for _, n := range st.Nodes {
		m.nodes[n.ID] = &nodeRuntime{
			id:         n.ID,
			name:       n.Name,
			serial:     n.Serial,
			enrolledAt: n.EnrolledAt,
			revoked:    n.Revoked,
			revokedAt:  n.RevokedAt,
			notify:     make(chan struct{}, 1),
		}
	}
	for nonce, exp := range st.UsedTokens {
		m.usedTokens[nonce] = exp
	}
	return nil
}

// persistLocked 把身份与已用令牌落盘。调用方必须持有 m.mu。
func (m *master) persistLocked() {
	st := persistedState{UsedTokens: make(map[string]int64, len(m.usedTokens))}
	for _, n := range m.nodes {
		st.Nodes = append(st.Nodes, persistedNode{
			ID: n.id, Name: n.name, Serial: n.serial,
			EnrolledAt: n.enrolledAt, Revoked: n.revoked, RevokedAt: n.revokedAt,
		})
	}
	sort.Slice(st.Nodes, func(i, j int) bool { return st.Nodes[i].ID < st.Nodes[j].ID })
	for nonce, exp := range m.usedTokens {
		st.UsedTokens[nonce] = exp
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(m.statePath(), data, 0o600)
}

// pruneTokensLocked 清掉过期的已用令牌，避免状态文件无限增长。
func (m *master) pruneTokensLocked(now time.Time) {
	for nonce, exp := range m.usedTokens {
		if exp < now.Unix() {
			delete(m.usedTokens, nonce)
		}
	}
}

func (m *master) taskDir() string { return filepath.Join(m.dir, "tasks") }

func (m *master) persistTask(v taskView) {
	if err := os.MkdirAll(m.taskDir(), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = writeFileAtomic(filepath.Join(m.taskDir(), v.ID+".json"), data, 0o600)
}

// loadHistory 在启动时把历史任务读回内存，运维重启主控后仍能在控制台看到结论。
func (m *master) loadHistory() {
	entries, err := os.ReadDir(m.taskDir())
	if err != nil {
		return
	}
	views := make([]taskView, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.taskDir(), e.Name()))
		if err != nil {
			continue
		}
		var v taskView
		if err := json.Unmarshal(data, &v); err != nil || v.ID == "" {
			continue
		}
		v.State = finalizedLabel(v.State)
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].StartedAt.After(views[j].StartedAt) })
	if len(views) > distKeptTasks {
		views = views[:distKeptTasks]
	}
	m.mu.Lock()
	m.history = views
	m.mu.Unlock()
}

func finalizedLabel(s string) string {
	switch s {
	case taskDone, taskStopped, taskDegraded:
		return s
	}
	return taskDone
}

// ---------- HTTP 层 ----------

func (m *master) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleIndex)
	mux.HandleFunc("/static/app.css", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, "dist_console.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("/static/app.js", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, "dist_console.js", "application/javascript; charset=utf-8")
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("/api/v1/", m.handleAPI)
	return m.withSecurityHeaders(mux)
}

func (m *master) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// 页面只允许加载自己域内的脚本与样式，禁掉内联脚本：
		// 即使某处渲染出现注入，也拿不到执行脚本的机会。
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
				"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; "+
				"base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (m *master) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serveAsset(w, r, "dist_console.html", "text/html; charset=utf-8")
}

func (m *master) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/api/v1/enroll":
		m.handleEnroll(w, r)
	case path == "/api/v1/login":
		m.handleLogin(w, r)
	case path == "/api/v1/logout":
		m.handleLogout(w, r)
	case path == "/api/v1/session":
		m.handleSession(w, r)
	case path == "/api/v1/nodes":
		m.handleNodes(w, r)
	case strings.HasPrefix(path, "/api/v1/nodes/"):
		m.handleNodeAction(w, r, strings.TrimPrefix(path, "/api/v1/nodes/"))
	case path == "/api/v1/tasks":
		m.handleTasks(w, r)
	case strings.HasPrefix(path, "/api/v1/tasks/"):
		m.handleTaskAction(w, r, strings.TrimPrefix(path, "/api/v1/tasks/"))
	case path == "/api/v1/join":
		m.handleJoin(w, r)
	case path == "/api/v1/agent/poll":
		m.handleAgentPoll(w, r)
	case path == "/api/v1/agent/progress":
		m.handleAgentProgress(w, r)
	case path == "/api/v1/agent/result":
		m.handleAgentResult(w, r)
	case path == "/api/v1/agent/renew":
		m.handleAgentRenew(w, r)
	default:
		writeError(w, http.StatusNotFound, "未知接口")
	}
}

// serve 在已绑定的监听器上提供 TLS 服务。
// 监听放在调用方是为了先拿到端口、再打印"可用的地址"，端口冲突时不会误导运维。
func (m *master) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           m.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 长轮询最长等 20 秒，写超时必须比它大，否则命令还没发出连接就被关了。
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 << 10,
		ErrorLog:       nil,
	}
	tlsLn := tls.NewListener(ln, m.tlsConfig)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(tlsLn) }()

	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
}

// ---------- 节点入网与控制面 ----------

func (m *master) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	ip := clientIP(r)
	if !m.enroll.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "加入申请过于频繁，请稍后再试")
		return
	}
	var req enrollRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	pl, err := m.pki.verifyJoinToken(req.Token)
	if err != nil {
		m.audit.event("enroll_rejected", map[string]any{"ip": ip, "reason": err.Error()})
		writeError(w, http.StatusForbidden, "加入令牌无效或已过期，请在主控控制台重新签发")
		return
	}
	if strings.TrimSpace(req.CSR) == "" {
		writeError(w, http.StatusBadRequest, "缺少 CSR")
		return
	}

	// 单次使用：先占住 nonce 再签发。放在同一个临界区里，
	// 两个并发请求就不可能同时通过（否则一个令牌能入网两台机器）。
	now := time.Now()
	m.mu.Lock()
	if _, used := m.usedTokens[pl.Nonce]; used {
		m.mu.Unlock()
		m.audit.event("enroll_replay", map[string]any{"ip": ip})
		writeError(w, http.StatusForbidden, "加入令牌已被使用")
		return
	}
	if len(m.nodes) >= m.maxNodes {
		m.mu.Unlock()
		writeError(w, http.StatusForbidden, "节点数量已达上限")
		return
	}
	m.usedTokens[pl.Nonce] = pl.Exp
	m.pruneTokensLocked(now)
	m.mu.Unlock()

	nodeID := "node-" + randomHex(4)
	certPEM, serial, err := m.pki.issueClientCert(req.CSR, nodeID)
	if err != nil {
		m.mu.Lock()
		delete(m.usedTokens, pl.Nonce)
		m.mu.Unlock()
		m.audit.event("enroll_rejected", map[string]any{"ip": ip, "reason": err.Error()})
		writeError(w, http.StatusBadRequest, "CSR 无效："+err.Error())
		return
	}

	m.mu.Lock()
	m.nodes[nodeID] = &nodeRuntime{
		id:         nodeID,
		name:       sanitizeNodeName(req.Name),
		serial:     serial,
		enrolledAt: now,
		lastSeen:   now,
		version:    sanitizeNodeName(req.Version),
		os:         sanitizeNodeName(req.OS),
		arch:       sanitizeNodeName(req.Arch),
		cpus:       req.CPUs,
		remoteAddr: ip,
		notify:     make(chan struct{}, 1),
	}
	m.persistLocked()
	m.mu.Unlock()

	m.audit.event("enroll", map[string]any{
		"node": nodeID, "name": sanitizeNodeName(req.Name), "ip": ip,
		"version": req.Version, "serial": serial,
	})
	writeJSON(w, http.StatusOK, enrollResponse{
		NodeID:     nodeID,
		CertPEM:    string(certPEM),
		CAPEM:      string(m.pki.certPEM),
		ServerTime: time.Now().UTC(),
	})
}

// agentNodeID 校验客户端证书并把请求绑定到一个已注册、未吊销的节点。
// 注意 TLS 层只保证"证书链有效"；"这个节点还活着、没被吊销、证书没被换过"
// 是应用层必须自己回答的问题。
func (m *master) agentNodeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		writeError(w, http.StatusUnauthorized, "需要有效的客户端证书")
		return "", false
	}
	cert := r.TLS.PeerCertificates[0]
	id := cert.Subject.CommonName
	m.mu.Lock()
	n := m.nodes[id]
	if n == nil {
		m.mu.Unlock()
		writeError(w, http.StatusForbidden, "节点未注册，请重新加入集群")
		return "", false
	}
	if n.revoked {
		m.mu.Unlock()
		writeError(w, http.StatusForbidden, "节点已被吊销")
		return "", false
	}
	// 证书序列号与登记值不一致，说明该节点换过证书（或是旧证书被复制到了别处）。
	if n.serial != cert.SerialNumber.Text(16) {
		m.mu.Unlock()
		writeError(w, http.StatusForbidden, "证书已失效，请重新加入集群")
		return "", false
	}
	m.mu.Unlock()
	return id, true
}

func (m *master) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	nodeID, ok := m.agentNodeID(w, r)
	if !ok {
		return
	}
	var req pollRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if req.Proto != distProtoVersion {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"协议版本不一致：主控 %d，节点 %d，请让主从使用同一版本的可执行文件",
			distProtoVersion, req.Proto))
		return
	}
	m.touchNode(nodeID, req, clientIP(r))

	deadline := time.Now().Add(m.pollWait)
	for {
		cmd, notify, alive := m.takeCommand(nodeID)
		if !alive {
			writeError(w, http.StatusForbidden, "节点不可用")
			return
		}
		if cmd != nil {
			writeJSON(w, http.StatusOK, pollResponse{Command: cmd, ServerTime: time.Now().UTC()})
			return
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			writeJSON(w, http.StatusOK, pollResponse{ServerTime: time.Now().UTC()})
			return
		}
		timer := time.NewTimer(remain)
		select {
		case <-notify:
		case <-timer.C:
		case <-r.Context().Done():
		}
		timer.Stop()
	}
}

func (m *master) handleAgentProgress(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	nodeID, ok := m.agentNodeID(w, r)
	if !ok {
		return
	}
	var p Progress
	if err := decodeJSONBody(w, r, &p); err != nil {
		return
	}
	stop, byDeadline, known := m.applyProgress(nodeID, p)
	reason := ""
	switch {
	case !stop:
	case byDeadline:
		reason = "deadline"
	default:
		reason = "stopped"
	}
	writeJSON(w, http.StatusOK, map[string]any{"known": known, "stop": stop, "reason": reason})
}

func (m *master) handleAgentResult(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	nodeID, ok := m.agentNodeID(w, r)
	if !ok {
		return
	}
	var res TaskResult
	if err := decodeJSONBody(w, r, &res); err != nil {
		return
	}
	m.applyResult(nodeID, res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *master) handleAgentRenew(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	nodeID, ok := m.agentNodeID(w, r)
	if !ok {
		return
	}
	var req renewRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	certPEM, serial, err := m.pki.issueClientCert(req.CSR, nodeID)
	if err != nil {
		m.audit.event("renew_rejected", map[string]any{"node": nodeID, "reason": err.Error()})
		writeError(w, http.StatusBadRequest, "CSR 无效："+err.Error())
		return
	}
	m.mu.Lock()
	if n := m.nodes[nodeID]; n != nil {
		n.serial = serial
		n.lastSeen = time.Now()
		m.persistLocked()
	}
	m.mu.Unlock()
	m.audit.event("renew", map[string]any{"node": nodeID, "serial": serial})
	writeJSON(w, http.StatusOK, renewResponse{
		NodeID:     nodeID,
		CertPEM:    string(certPEM),
		ExpiresAt:  time.Now().Add(distAgentCertTTL).UTC(),
		ServerTime: time.Now().UTC(),
	})
}

func (m *master) touchNode(nodeID string, req pollRequest, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[nodeID]
	if n == nil {
		return
	}
	n.lastSeen = time.Now()
	n.remoteAddr = ip
	n.running = sanitizeNodeName(req.Running)
	if v := sanitizeNodeName(req.Version); v != "" {
		n.version = v
	}
	if v := sanitizeNodeName(req.OS); v != "" {
		n.os = v
	}
	if v := sanitizeNodeName(req.Arch); v != "" {
		n.arch = v
	}
	if req.CPUs > 0 {
		n.cpus = req.CPUs
	}
}

func (m *master) takeCommand(nodeID string) (*Command, <-chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[nodeID]
	if n == nil || n.revoked {
		return nil, nil, false
	}
	if len(n.pending) == 0 {
		return nil, n.notify, true
	}
	cmd := n.pending[0]
	n.pending = n.pending[1:]
	return &cmd, n.notify, true
}

// enqueueLocked 把命令排进节点的待发队列并唤醒它的长轮询。
// 调用方必须持有 m.mu。
func (m *master) enqueueLocked(nodeID string, cmd Command) {
	n := m.nodes[nodeID]
	if n == nil || n.revoked {
		return
	}
	for _, p := range n.pending {
		if p.Kind == cmd.Kind && p.TaskID == cmd.TaskID {
			return // 同一条命令只排一次，避免重复 start/stop
		}
	}
	n.pending = append(n.pending, cmd)
	if len(n.pending) > 32 {
		n.pending = n.pending[len(n.pending)-32:]
	}
	select {
	case n.notify <- struct{}{}:
	default:
	}
}

func (m *master) nodeOnlineLocked(n *nodeRuntime) bool {
	return n != nil && !n.revoked && !n.lastSeen.IsZero() && time.Since(n.lastSeen) <= distOfflineAfter
}

func (m *master) busyTaskLocked(nodeID string) string {
	for id, t := range m.tasks {
		if t.finalized {
			continue
		}
		if sh := t.shards[nodeID]; sh != nil && !shardTerminal(sh.state) {
			return id
		}
	}
	return ""
}

// ---------- 任务 ----------

// applyProgress 收下一帧进度，并告诉节点是否该停、以及"为什么停"。
//
// byDeadline 必须和"人工停止"分开：任务到点时主控自己也会回 stop，
// 如果节点把它当成被叫停，一条跑满时长的正常任务会被标成 stopped，
// 控制台上的历史就会变成一片"已停止"。
func (m *master) applyProgress(nodeID string, p Progress) (stop, byDeadline, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[p.TaskID]
	if t == nil || t.finalized {
		return true, false, false
	}
	sh := t.shards[nodeID]
	if sh == nil {
		return true, false, false
	}
	now := time.Now()
	// 同一 seq 或更旧的包直接忽略：重传、乱序都不该让速率算出负数。
	if p.Seq <= sh.seq {
		userStop := t.state != taskRunning
		expired := !userStop && now.After(t.endsAt)
		return userStop || expired, expired, true
	}
	sh.seq = p.Seq
	if !sh.prevAt.IsZero() {
		if dt := now.Sub(sh.prevAt).Seconds(); dt >= 0.2 {
			if p.Requests >= sh.prevReq {
				sh.rps = float64(p.Requests-sh.prevReq) / dt
			}
			if p.Bytes >= sh.prevBytes {
				sh.bps = float64(p.Bytes-sh.prevBytes) / dt
			}
		}
	}
	sh.prevAt, sh.prevReq, sh.prevBytes = now, p.Requests, p.Bytes
	sh.requests, sh.failed, sh.bytes = p.Requests, p.Failed, p.Bytes
	if p.Conc > 0 {
		sh.conc = p.Conc
	}
	sh.lastError = shortText(p.LastError, 200)
	sh.lastAt = now
	if sh.state == shardAssigned {
		sh.state = shardRunning
	}
	m.appendPointLocked(t, now)
	userStop := t.state != taskRunning
	expired := !userStop && now.After(t.endsAt)
	return userStop || expired, expired, true
}

func (m *master) applyResult(nodeID string, res TaskResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[res.TaskID]
	if t == nil || t.finalized {
		return
	}
	sh := t.shards[nodeID]
	if sh == nil {
		return
	}
	sh.requests, sh.failed, sh.bytes = res.Requests, res.Failed, res.Bytes
	sh.rps, sh.bps = 0, 0
	sh.lastAt = time.Now()
	if res.Error != "" {
		sh.lastError = shortText(res.Error, 200)
	}
	switch res.Status {
	case taskDone:
		sh.state = shardDone
	case taskStopped:
		sh.state = shardStopped
	default:
		sh.state = shardErrored
	}
	m.maybeFinalizeLocked(t)
}

func (m *master) appendPointLocked(t *taskRuntime, now time.Time) {
	if !t.lastPoint.IsZero() && now.Sub(t.lastPoint) < 700*time.Millisecond {
		return
	}
	var rps, bps float64
	for _, sh := range t.shards {
		if sh.state == shardRunning || sh.state == shardAssigned {
			rps += sh.rps
			bps += sh.bps
		}
	}
	if rps > t.peakRPS {
		t.peakRPS = rps
	}
	t.lastPoint = now
	t.series = append(t.series, seriesPoint{T: now.Sub(t.started).Milliseconds(), RPS: rps, BPS: bps})
	if len(t.series) > distSeriesMax {
		t.series = t.series[len(t.series)-distSeriesMax:]
	}
}

func (m *master) createTask(req taskCreateRequest, by string) (taskView, error) {
	spec := req.TaskSpec
	spec.normalize()

	if len(req.NodeIDs) == 0 {
		if req.TotalConc <= 0 {
			req.TotalConc = 8
		}
	}

	m.mu.Lock()
	ids := make([]string, 0, len(req.NodeIDs))
	if len(req.NodeIDs) == 0 {
		for id, n := range m.nodes {
			if m.nodeOnlineLocked(n) && m.busyTaskLocked(id) == "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	} else {
		for _, id := range req.NodeIDs {
			n := m.nodes[id]
			if n == nil {
				m.mu.Unlock()
				return taskView{}, fmt.Errorf("节点 %s 不存在", id)
			}
			if n.revoked {
				m.mu.Unlock()
				return taskView{}, fmt.Errorf("节点 %s 已被吊销", id)
			}
			if !m.nodeOnlineLocked(n) {
				m.mu.Unlock()
				return taskView{}, fmt.Errorf("节点 %s 不在线（最后心跳超过 %s）", id, distOfflineAfter)
			}
			if busy := m.busyTaskLocked(id); busy != "" {
				m.mu.Unlock()
				return taskView{}, fmt.Errorf("节点 %s 正在执行任务 %s", id, busy)
			}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		m.mu.Unlock()
		return taskView{}, fmt.Errorf("没有可用节点：请先让从节点加入，且保持心跳在 %s 内", distOfflineAfter)
	}
	if len(ids) > distMaxNodesPerTask {
		ids = ids[:distMaxNodesPerTask]
	}

	per := splitPerNode(req.TotalConc, len(ids))
	spec.PerNodeConc = per
	// 实际总并发由"每节点并发 × 节点数"决定；把请求值直接留住
	// 会让控制台显示一个不存在的总数（尤其是被 256 上限夹住时）。
	spec.TotalConc = per * len(ids)
	if err := spec.validate(); err != nil {
		m.mu.Unlock()
		return taskView{}, err
	}

	now := time.Now()
	t := &taskRuntime{
		spec:    spec,
		state:   taskRunning,
		started: now,
		endsAt:  now.Add(time.Duration(spec.DurationSec) * time.Second),
		shards:  make(map[string]*shardRuntime, len(ids)),
		done:    make(chan struct{}),
	}
	// lastPoint 故意留零值：第一个采样点必须立刻进曲线，
	// 否则控制台在任务开始后的第一秒里是一张空图。
	for _, id := range ids {
		t.shards[id] = &shardRuntime{nodeID: id, conc: per, state: shardAssigned, lastAt: now}
		dispatch := spec
		m.enqueueLocked(id, Command{Kind: cmdStart, TaskID: spec.ID, Spec: &dispatch})
	}
	m.tasks[spec.ID] = t
	m.order = append(m.order, spec.ID)
	if len(m.order) > distKeptTasks {
		m.order = m.order[len(m.order)-distKeptTasks:]
	}
	m.trimTasksLocked()
	view := m.taskViewLocked(t)
	m.mu.Unlock()

	m.audit.event("task_start", map[string]any{
		"task": spec.ID, "by": by, "target": spec.Target, "proto": spec.Proto,
		"durationSec": spec.DurationSec, "perNodeConc": per, "nodes": ids,
	})
	go m.watchTask(t)
	return view, nil
}

// watchTask 是任务的收尾推进器：到点发停止命令、把长时间沉默的节点判死、
// 所有节点都有结论后落盘。没有它，一个掉线节点会让任务永远停在 running。
func (m *master) watchTask(t *taskRuntime) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case now := <-tick.C:
			m.mu.Lock()
			if t.finalized {
				m.mu.Unlock()
				return
			}
			stopDeadline := t.endsAt.Add(distStopGrace)
			for id, sh := range t.shards {
				if shardTerminal(sh.state) {
					continue
				}
				if (t.state != taskRunning || now.After(t.endsAt)) && !sh.stopSent {
					m.enqueueLocked(id, Command{Kind: cmdStop, TaskID: t.spec.ID})
					sh.stopSent = true
				}
				if now.After(stopDeadline) {
					sh.state = shardLost
					sh.rps, sh.bps = 0, 0
					if sh.lastError == "" {
						sh.lastError = "节点在截止时间后没有上报结果"
					}
					continue
				}
				if sh.state == shardRunning && !sh.lastAt.IsZero() && now.Sub(sh.lastAt) > distLostAfter {
					sh.state = shardLost
					sh.rps, sh.bps = 0, 0
					if sh.lastError == "" {
						sh.lastError = "节点超过 25 秒没有上报进度"
					}
				}
			}
			m.maybeFinalizeLocked(t)
			final := t.finalized
			m.mu.Unlock()
			if final {
				return
			}
		}
	}
}

func (m *master) maybeFinalizeLocked(t *taskRuntime) {
	if t.finalized {
		return
	}
	for _, sh := range t.shards {
		if !shardTerminal(sh.state) {
			return
		}
	}
	t.finalized = true
	close(t.done)
	if t.state == taskRunning {
		// 主动停止与"节点丢失"要能一眼区分：停止是运维的意图，
		// degraded 才是"这次测量不完整"。
		degraded := false
		for _, sh := range t.shards {
			if sh.state == shardLost || sh.state == shardErrored {
				degraded = true
			}
		}
		if degraded {
			t.state = taskDegraded
		} else {
			t.state = taskDone
		}
	}
	view := m.taskViewLocked(t)
	m.pushHistoryLocked(view)
	m.audit.event("task_finish", map[string]any{
		"task": t.spec.ID, "state": t.state,
		"requests": view.Totals.Requests, "failed": view.Totals.Failed,
		"bytes": view.Totals.Bytes, "peakRps": view.Totals.PeakRPS,
	})
	go m.persistTask(view)
}

func (m *master) pushHistoryLocked(v taskView) {
	m.history = append(m.history, v)
	if len(m.history) > distKeptTasks {
		m.history = m.history[len(m.history)-distKeptTasks:]
	}
}

func (m *master) stopTask(id, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return fmt.Errorf("任务 %s 不存在", id)
	}
	if t.finalized {
		return fmt.Errorf("任务 %s 已经结束", id)
	}
	if t.state == taskRunning {
		t.state = taskStopped
		t.stoppedBy = by
	}
	for nodeID, sh := range t.shards {
		if shardTerminal(sh.state) || sh.stopSent {
			continue
		}
		m.enqueueLocked(nodeID, Command{Kind: cmdStop, TaskID: id})
		sh.stopSent = true
	}
	m.audit.event("task_stop", map[string]any{"task": id, "by": by})
	return nil
}

// trimTasksLocked 只保留最近 distKeptTasks 条任务的运行态。
// 长时间运行的主控如果一直留着已结束任务的副本，内存会随发版次数线性增长。
func (m *master) trimTasksLocked() {
	keep := make(map[string]bool, len(m.order))
	for _, id := range m.order {
		keep[id] = true
	}
	for id, t := range m.tasks {
		if !keep[id] && t.finalized {
			delete(m.tasks, id)
		}
	}
}

func (m *master) taskViewLocked(t *taskRuntime) taskView {
	now := time.Now()
	v := taskView{
		ID:          t.spec.ID,
		Target:      t.spec.Target,
		Proto:       t.spec.Proto,
		DurationSec: t.spec.DurationSec,
		TotalConc:   t.spec.TotalConc,
		PerNodeConc: t.spec.PerNodeConc,
		NoReuse:     t.spec.NoReuse,
		ProxySpoof:  t.spec.ProxySpoof,
		XForwardFor: t.spec.XForwardFor,
		RandomPost:  t.spec.RandomPost,
		RandomSize:  t.spec.RandomSize,
		Headers:     t.spec.Headers,
		Family:      t.spec.Family,
		TimeoutSec:  t.spec.TimeoutSec,
		Note:        t.spec.Note,
		State:       t.state,
		StartedAt:   t.started,
		EndsAt:      t.endsAt,
		StoppedBy:   t.stoppedBy,
		ElapsedMs:   now.Sub(t.started).Milliseconds(),
	}
	if remain := t.endsAt.Sub(now); remain > 0 {
		v.RemainingMs = remain.Milliseconds()
	}
	nodes := make([]shardView, 0, len(t.shards))
	totals := taskTotals{PeakRPS: t.peakRPS}
	for _, sh := range t.shards {
		n := m.nodes[sh.nodeID]
		name := ""
		if n != nil {
			name = n.name
		}
		seen := 0
		if !sh.lastAt.IsZero() {
			seen = int(time.Since(sh.lastAt).Seconds())
		}
		sv := shardView{
			NodeID: sh.nodeID, Name: name, State: sh.state, Conc: sh.conc,
			Requests: sh.requests, Failed: sh.failed, Bytes: sh.bytes,
			LastError: sh.lastError, LastSeenAgoSec: seen,
		}
		if sh.state == shardRunning {
			sv.RPS, sv.BPS = sh.rps, sh.bps
		}
		nodes = append(nodes, sv)
		totals.Requests += sh.requests
		totals.Failed += sh.failed
		totals.Bytes += sh.bytes
		totals.RPS += sv.RPS
		totals.BPS += sv.BPS
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].NodeID < nodes[j].NodeID
	})
	if totals.RPS > totals.PeakRPS {
		totals.PeakRPS = totals.RPS
	}
	// 峰值不可能低于平均值。短任务可能只采到一两个数据点，
	// 只靠采样求峰值会出现"峰值 0 但请求数上万"的荒谬组合。
	if secs := v.ElapsedMs / 1000; secs > 0 {
		if avg := float64(totals.Requests) / float64(secs); avg > totals.PeakRPS {
			totals.PeakRPS = avg
		}
	}
	v.Nodes = nodes
	v.Totals = totals
	v.Series = append([]seriesPoint(nil), t.series...)
	return v
}

// ---------- 控制台接口 ----------

func (m *master) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	ip := clientIP(r)
	if d := m.login.blocked(ip); d > 0 {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(d.Seconds())))
		return
	}
	if !m.login.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if !checkAdminPassword(m.adminHash, req.Password) {
		blocked := m.login.fail(ip)
		m.audit.event("login_failed", map[string]any{"ip": ip})
		if blocked > 0 {
			writeError(w, http.StatusTooManyRequests, fmt.Sprintf("密码错误次数过多，请 %d 秒后再试", int(blocked.Seconds())))
			return
		}
		writeError(w, http.StatusUnauthorized, "密码不正确")
		return
	}
	m.login.clear(ip)
	token, csrf := m.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     distSessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(distSessionTTL.Seconds()),
	})
	m.audit.event("login_ok", map[string]any{"ip": ip, "session": bearerHash(token)})
	writeJSON(w, http.StatusOK, map[string]any{"csrf": csrf})
}

func (m *master) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if _, ok := m.requireAdmin(w, r, true); !ok {
		return
	}
	if c, err := r.Cookie(distSessionCookie); err == nil {
		m.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: distSessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *master) handleSession(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	m.mu.Lock()
	nodeCount := len(m.nodes)
	m.mu.Unlock()
	info := map[string]any{
		"version":     version,
		"proto":       distProtoVersion,
		"fingerprint": m.pki.displayFingerprint(),
		"uptimeSec":   int(time.Since(m.started).Seconds()),
		"nodeCount":   nodeCount,
	}
	if sess, ok := m.currentSession(r); ok {
		info["authenticated"] = true
		info["csrf"] = sess.csrf
	} else {
		info["authenticated"] = false
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *master) currentSession(r *http.Request) (*adminSession, bool) {
	c, err := r.Cookie(distSessionCookie)
	if err != nil {
		return nil, false
	}
	return m.sessions.lookup(c.Value)
}

// requireAdmin 校验登录态；needCSRF 为真时还要求 CSRF 头匹配。
// 所有会改变主控状态的请求都必须带 needCSRF=true。
func (m *master) requireAdmin(w http.ResponseWriter, r *http.Request, needCSRF bool) (*adminSession, bool) {
	sess, ok := m.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return nil, false
	}
	if needCSRF {
		got := r.Header.Get(distCSRFHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
			writeError(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面")
			return nil, false
		}
	}
	return sess, true
}

func (m *master) handleNodes(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := m.requireAdmin(w, r, false); !ok {
		return
	}
	m.mu.Lock()
	views := make([]nodeView, 0, len(m.nodes))
	now := time.Now()
	for id, n := range m.nodes {
		v := nodeView{
			ID: id, Name: n.name, Revoked: n.revoked,
			Version: n.version, OS: n.os, Arch: n.arch, CPUs: n.cpus,
			Online: m.nodeOnlineLocked(n), Running: n.running, EnrolledAt: n.enrolledAt,
		}
		if !n.lastSeen.IsZero() {
			v.LastSeenAgoSec = int(now.Sub(n.lastSeen).Seconds())
		}
		for _, t := range m.tasks {
			if t.finalized {
				continue
			}
			if sh := t.shards[id]; sh != nil && (sh.state == shardRunning || sh.state == shardAssigned) {
				v.RPS, v.BPS = sh.rps, sh.bps
			}
		}
		views = append(views, v)
	}
	m.mu.Unlock()
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].ID < views[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

func (m *master) handleNodeAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "revoke" {
		writeError(w, http.StatusNotFound, "未知接口")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if _, ok := m.requireAdmin(w, r, true); !ok {
		return
	}
	id := parts[0]
	m.mu.Lock()
	n := m.nodes[id]
	if n == nil {
		m.mu.Unlock()
		writeError(w, http.StatusNotFound, "节点不存在")
		return
	}
	n.revoked = true
	n.revokedAt = time.Now()
	n.pending = nil
	m.persistLocked()
	m.mu.Unlock()

	m.audit.event("node_revoke", map[string]any{"node": id, "ip": clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *master) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := m.requireAdmin(w, r, false); !ok {
			return
		}
		m.mu.Lock()
		list := make([]taskView, 0, len(m.tasks)+len(m.history))
		for _, t := range m.tasks {
			// 已结束的任务已经在 history 里，这里跳过，否则控制台会看到两条一样的记录。
			if t.finalized {
				continue
			}
			list = append(list, m.taskViewLocked(t))
		}
		list = append(list, m.history...)
		m.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].StartedAt.After(list[j].StartedAt) })
		if len(list) > distKeptTasks {
			list = list[:distKeptTasks]
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": list})
	case http.MethodPost:
		sess, ok := m.requireAdmin(w, r, true)
		if !ok {
			return
		}
		var req taskCreateRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			return
		}
		view, err := m.createTask(req, "admin:"+bearerHash(sess.csrf))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET / POST")
	}
}

func (m *master) handleTaskAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "缺少任务 ID")
		return
	}
	if len(parts) == 2 && parts[1] == "stop" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if _, ok := m.requireAdmin(w, r, true); !ok {
			return
		}
		if err := m.stopTask(id, "admin"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "未知接口")
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := m.requireAdmin(w, r, false); !ok {
		return
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		v := m.taskViewLocked(t)
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, v)
		return
	}
	for _, v := range m.history {
		if v.ID == id {
			m.mu.Unlock()
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	m.mu.Unlock()
	writeError(w, http.StatusNotFound, "任务不存在")
}

func (m *master) handleJoin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if _, ok := m.requireAdmin(w, r, true); !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	info, err := m.newJoin(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.audit.event("join_issue", map[string]any{"url": info.URL, "expiresAt": info.ExpiresAt})
	writeJSON(w, http.StatusOK, map[string]any{
		"join":        info.encode(),
		"command":     "webBenchmark -join " + info.encode(),
		"url":         info.URL,
		"token":       info.Token,
		"fingerprint": m.pki.displayFingerprint(),
		"expiresAt":   info.ExpiresAt,
	})
}

// newJoin 签发一个新的入网凭据。URL 允许运维填写，因为主控监听 0.0.0.0 时
// 无法自己猜出从节点能看到哪个地址。
func (m *master) newJoin(rawURL string) (joinInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return joinInfo{}, fmt.Errorf("缺少主控地址")
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return joinInfo{}, fmt.Errorf("主控地址必须以 https:// 开头")
	}
	rawURL = strings.TrimSuffix(rawURL, "/")
	tok, pl, err := m.pki.newJoinToken(m.joinTTL)
	if err != nil {
		return joinInfo{}, err
	}
	info := joinInfo{
		V:           distProtoVersion,
		URL:         rawURL,
		Token:       tok,
		Fingerprint: m.pki.fp,
		ExpiresAt:   time.Unix(pl.Exp, 0).UTC(),
	}
	encoded := info.encode()
	if encoded == "" {
		return joinInfo{}, fmt.Errorf("生成加入凭据失败")
	}
	return info, nil
}

// ---------- 小工具 ----------

func serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	data := consoleAsset(name)
	if data == nil {
		http.NotFound(w, r)
		return
	}
	etag := `"` + sha256Hex(data)[:16] + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, distMaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return err
	}
	return nil
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, "只支持 "+method)
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
