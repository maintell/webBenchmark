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
	// conn 非空即在线。命令直接写进这条连接，不再有"待发队列 + 长轮询"。
	conn *agentConn
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
	p, err := newPKI(opts.Dir)
	if err != nil {
		return nil, err
	}
	cert, err := p.issueServerCert(opts.Hosts)
	if err != nil {
		return nil, err
	}
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
			// 只上 TLS 1.3：这套控制面只服务本工具自己的从节点和现代浏览器。
			MinVersion: tls.VersionTLS13,
			// 不要求客户端证书：身份认证已经下沉到应用层握手（见 dist_secure.go），
			// 这样挂在 TLS 终止代理后面时也不会因为"证书到不了进程"而失效。
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

// masterPublicURL 给出加入命令里默认填写的主控地址。
// 监听在 0.0.0.0 时无法知道运维会从哪个地址访问，用主机名做最佳猜测，
// 控制台里允许改成实际可达的地址。
func masterPublicURL(scheme, bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return scheme + "://" + bind
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		if hn, err := os.Hostname(); err == nil && hn != "" {
			host = hn
		} else {
			host = "127.0.0.1"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
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
	case path == "/api/v1/ssh/install":
		m.handleSSHInstall(w, r)
	case path == "/api/v1/ssh/probe":
		m.handleSSHProbe(w, r)
	case path == "/api/v1/agent/ws":
		// 主从之间唯一的入口：一条加密的双向 WebSocket。
		m.handleAgentWS(w, r)
	default:
		writeError(w, http.StatusNotFound, "未知接口")
	}
}

// serve 在已绑定的监听器上提供服务。tlsConf 为 nil 表示纯 http（运维显式选择）。
// 监听放在调用方是为了先拿到端口、再打印"可用的地址"，端口冲突时不会误导运维。
func (m *master) serve(ctx context.Context, ln net.Listener, tlsConf *tls.Config) error {
	srv := &http.Server{
		Handler:           m.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WebSocket 是长连接，靠自身心跳存活；这里只兜住普通请求。
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 << 10,
		ErrorLog:       nil,
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		// 已升级（hijacked）的 WebSocket 不在 Shutdown 的管理范围内，
		// 必须先主动断开，否则主控退出会卡满一个读超时。
		m.closeAgentConns()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
}

// ---------- 节点入网与控制面 ----------
//
// 入网、心跳、命令、进度、结果、换证现在全部走 /api/v1/agent/ws 一条加密连接，
// 具体实现见 dist_agentapi.go 与 dist_secure.go。

func (m *master) nodeOnlineLocked(n *nodeRuntime) bool {
	// 在线 = 有一条活着的会话。心跳只用来刷新元数据与"多久没动静"的展示，
	// 不再参与判定：连接断了就是离线，没有中间态。
	return n != nil && !n.revoked && n.conn != nil
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
// applyProgress 收下一帧进度。
//
// 是否要停不再由这一帧回答：主控的看门狗在到点或人工停止时主动下发 stop，
// 节点自己也知道时长，两边各有一份判据，网络抖动不会把正常完成判成中断。
func (m *master) applyProgress(nodeID string, p Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[p.TaskID]
	if t == nil || t.finalized {
		return
	}
	sh := t.shards[nodeID]
	if sh == nil {
		return
	}
	now := time.Now()
	// 同一 seq 或更旧的包直接忽略：重传、乱序都不该让速率算出负数。
	if p.Seq <= sh.seq {
		return
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
				return taskView{}, fmt.Errorf("节点 %s 不在线（没有活跃连接）", id)
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
		return taskView{}, fmt.Errorf("没有可用节点：请先让从节点加入并保持连接")
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
	}
	for _, id := range ids {
		dispatch := spec
		if !m.enqueueLocked(id, wsControl{T: "start", TaskID: spec.ID, Spec: &dispatch}) {
			// 连接刚好在这两步之间断开：这个分片注定收不到结果，
			// 直接判 lost，让任务能正常收尾而不是挂到超时。
			sh := t.shards[id]
			sh.state = shardLost
			sh.lastError = "节点连接已断开"
		}
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
					reason := "deadline"
					if t.state != taskRunning {
						reason = "stopped"
					}
					m.enqueueLocked(id, wsControl{T: "stop", TaskID: t.spec.ID, Reason: reason})
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
		m.enqueueLocked(nodeID, wsControl{T: "stop", TaskID: id, Reason: "stopped"})
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
		// 只在 TLS 下发 Secure：http 模式下带 Secure 的 Cookie 浏览器根本不会保存，
		// 表现就是"输入正确密码后仍停在登录页"。
		Secure:   r.TLS != nil,
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
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1,
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
	// 吊销要当场生效：把连接也断掉，节点下一次重连就会在握手阶段被拒绝，
	// 而不是继续用一条已经建立的会话收发命令。
	conn := n.conn
	n.conn = nil
	m.persistLocked()
	m.mu.Unlock()
	if conn != nil {
		conn.close()
	}

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
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return joinInfo{}, fmt.Errorf("主控地址必须以 http:// 或 https:// 开头")
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
