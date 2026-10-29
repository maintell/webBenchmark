package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAdminPassword = "test-admin-password"

// newTestMaster 起一个真实 TLS 控制面（httptest），
// 证书与校验规则和生产一致：只信任自建 CA，不要求客户端证书（身份在应用层握手）。
func newTestMaster(t *testing.T) (*master, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	hash, _, err := loadOrCreateAdminHash(dir, testAdminPassword)
	if err != nil {
		t.Fatalf("admin hash: %v", err)
	}
	m, err := newMaster(masterOptions{Dir: dir, JoinTTL: 15 * time.Minute, AdminHash: hash})
	if err != nil {
		t.Fatalf("newMaster: %v", err)
	}
	srv := httptest.NewUnstartedServer(m.handler())
	srv.TLS = m.tlsConfig
	srv.StartTLS()
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(func() {
		// 先断开已升级的 WebSocket，否则 Close 会一直等 handler 返回。
		m.closeAgentConns()
		srv.Close()
		m.close()
	})
	return m, srv
}

type testAdmin struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func newTestAdmin(t *testing.T, m *master, base string) *testAdmin {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    m.pki.caPool(),
		ServerName: distServerName,
		MinVersion: tls.VersionTLS13,
	}}
	a := &testAdmin{t: t, base: base, c: &http.Client{Transport: tr, Jar: jar}}
	status, body := a.raw(http.MethodPost, "/api/v1/login", map[string]string{"password": testAdminPassword}, nil)
	if status != http.StatusOK {
		t.Fatalf("login failed: %d %s", status, body)
	}
	var out struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.CSRF == "" {
		t.Fatalf("login response missing csrf: %s", body)
	}
	a.csrf = out.CSRF
	return a
}

func (a *testAdmin) raw(method, path string, body any, headers map[string]string) (int, []byte) {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// call 带 CSRF 头发请求，模拟控制台的真实调用。
func (a *testAdmin) call(method, path string, body any, out any) int {
	a.t.Helper()
	status, data := a.raw(method, path, body, map[string]string{distCSRFHeader: a.csrf})
	if out != nil && status == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			a.t.Fatalf("decode %s: %v (%s)", path, err, data)
		}
	}
	return status
}

// connectAgent 建立真实会话，并等到主控侧把它登记为在线。
func connectAgent(t *testing.T, m *master, a *agent) *agentSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := a.connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("connect %s: %v", a.name, err)
	}
	t.Cleanup(func() {
		sess.Close()
		cancel()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := m.nodes[a.state.NodeID]
		online := n != nil && n.conn != nil
		m.mu.Unlock()
		if online {
			return sess
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %s did not come online", a.state.NodeID)
	return nil
}

func TestJoinTokenIsSingleUse(t *testing.T) {
	m, srv := newTestMaster(t)
	first := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	connectAgent(t, m, first)

	cfg := first.cfg
	cfg.Dir = t.TempDir()
	second, err := newAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.connect(context.Background()); err == nil {
		t.Fatal("the same join token must not enroll a second node")
	}
}

func TestAgentWSRejectsBadToken(t *testing.T) {
	m, srv := newTestMaster(t)
	a, err := newAgent(agentConfig{
		Dir: t.TempDir(), URL: srv.URL, Name: "evil",
		Token: "wb1.bogus.sig", Fingerprint: m.pki.fp,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.connect(context.Background())
	if err == nil {
		t.Fatal("a bogus token must be rejected")
	}
	if !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("error should mention the token: %v", err)
	}
}

func TestRevokedNodeIsRejected(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	connectAgent(t, m, a)

	if status := admin.call(http.MethodPost, "/api/v1/nodes/"+a.state.NodeID+"/revoke", nil, nil); status != http.StatusOK {
		t.Fatalf("revoke failed: %d", status)
	}

	// 吊销要当场生效：已建立的连接被主控关闭，重连在握手阶段被拒。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.connect(ctx); err == nil {
		t.Fatal("a revoked node must not reconnect")
	} else if !strings.Contains(err.Error(), "吊销") {
		t.Fatalf("error should say the node was revoked: %v", err)
	}
}

func TestConsoleRequiresSessionAndCSRF(t *testing.T) {
	m, srv := newTestMaster(t)

	anon := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    m.pki.caPool(),
		ServerName: distServerName,
		MinVersion: tls.VersionTLS13,
	}}}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/nodes", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated node list should be 401, got %d", resp.StatusCode)
	}

	admin := newTestAdmin(t, m, srv.URL)
	if status := admin.call(http.MethodGet, "/api/v1/nodes", nil, nil); status != http.StatusOK {
		t.Fatalf("authenticated node list should be 200, got %d", status)
	}
	if status, _ := admin.raw(http.MethodPost, "/api/v1/tasks", taskCreateRequest{}, nil); status != http.StatusForbidden {
		t.Fatalf("POST without csrf should be 403, got %d", status)
	}
}

func TestLoginLockout(t *testing.T) {
	m, srv := newTestMaster(t)
	anon := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    m.pki.caPool(),
		ServerName: distServerName,
		MinVersion: tls.VersionTLS13,
	}}}

	for i := 1; i < distMaxLoginFails; i++ {
		status, _ := postRaw(t, anon, srv.URL+"/api/v1/login", map[string]string{"password": "wrong"})
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d should be 401, got %d", i, status)
		}
	}
	status, body := postRaw(t, anon, srv.URL+"/api/v1/login", map[string]string{"password": "wrong"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the source should be throttled, got %d %s", distMaxLoginFails, status, body)
	}
	status, _ = postRaw(t, anon, srv.URL+"/api/v1/login", map[string]string{"password": testAdminPassword})
	if status != http.StatusTooManyRequests {
		t.Fatalf("blocked source must stay blocked, got %d", status)
	}
}

func TestCreateTaskSplitsConcurrencyAndRejectsBusyNodes(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a1 := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	a2 := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a2")
	connectAgent(t, m, a1)
	connectAgent(t, m, a2)

	var view taskView
	status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec: TaskSpec{
			Target:      "https://example.com/",
			DurationSec: 30,
			TimeoutSec:  5,
			XForwardFor: true,
		},
		NodeIDs:   []string{a1.state.NodeID, a2.state.NodeID},
		TotalConc: 10,
	}, &view)
	if status != http.StatusOK {
		t.Fatalf("create task failed: %d", status)
	}
	if view.PerNodeConc != 5 {
		t.Fatalf("per node concurrency should be 5, got %d", view.PerNodeConc)
	}
	if len(view.Nodes) != 2 {
		t.Fatalf("expected 2 shards, got %d", len(view.Nodes))
	}

	status = admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec:  TaskSpec{Target: "https://example.com/", DurationSec: 30, TimeoutSec: 5},
		NodeIDs:   []string{a1.state.NodeID},
		TotalConc: 2,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("busy node should be rejected with 400, got %d", status)
	}

	// 收尾，避免看门狗一直跑
	m.applyResult(a1.state.NodeID, TaskResult{TaskID: view.ID, Status: taskDone, Requests: 1})
	m.applyResult(a2.state.NodeID, TaskResult{TaskID: view.ID, Status: taskDone, Requests: 1})
	m.mu.Lock()
	state := m.tasks[view.ID].state
	m.mu.Unlock()
	if state != taskDone {
		t.Fatalf("task should be done after every shard reported, got %s", state)
	}
}

func TestProgressAggregation(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a1 := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	a2 := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a2")
	connectAgent(t, m, a1)
	connectAgent(t, m, a2)

	var view taskView
	if status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec:  TaskSpec{Target: "https://example.com/", DurationSec: 60, TimeoutSec: 5, XForwardFor: true},
		NodeIDs:   []string{a1.state.NodeID, a2.state.NodeID},
		TotalConc: 4,
	}, &view); status != http.StatusOK {
		t.Fatalf("create task: %d", status)
	}

	m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 1, Requests: 100, Bytes: 1000})
	m.applyProgress(a2.state.NodeID, Progress{TaskID: view.ID, Seq: 1, Requests: 50, Bytes: 500})
	time.Sleep(250 * time.Millisecond)
	m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 200, Failed: 3, Bytes: 2000})
	m.applyProgress(a2.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 150, Failed: 1, Bytes: 1500})
	// 重复/乱序的包必须被忽略
	m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 1})

	var live taskView
	if status := admin.call(http.MethodGet, "/api/v1/tasks/"+view.ID, nil, &live); status != http.StatusOK {
		t.Fatalf("get task: %d", status)
	}
	if live.Totals.Requests != 350 || live.Totals.Failed != 4 || live.Totals.Bytes != 3500 {
		t.Fatalf("unexpected totals: %+v", live.Totals)
	}
	if live.Totals.RPS <= 0 {
		t.Fatalf("aggregate rps should be positive, got %f", live.Totals.RPS)
	}
	if len(live.Series) == 0 {
		t.Fatal("series should collect samples for the chart")
	}

	if status := admin.call(http.MethodPost, "/api/v1/tasks/"+view.ID+"/stop", nil, nil); status != http.StatusOK {
		t.Fatalf("stop: %d", status)
	}
	for _, id := range []string{a1.state.NodeID, a2.state.NodeID} {
		m.applyResult(id, TaskResult{TaskID: view.ID, Status: taskStopped})
	}
	m.mu.Lock()
	state := m.tasks[view.ID].state
	m.mu.Unlock()
	if state != taskStopped {
		t.Fatalf("task should end as stopped, got %s", state)
	}
}

// TestSupersededCertificateIsRejected 验证换发是原子替换：
// 新证书一登记，旧证书立刻在握手阶段被拒。
func TestSupersededCertificateIsRejected(t *testing.T) {
	m, srv := newTestMaster(t)
	a := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	connectAgent(t, m, a)
	oldCert := append([]byte{}, a.certPEM...)

	_, csr, err := newAgentCSR("a1")
	if err != nil {
		t.Fatal(err)
	}
	newCertPEM, serial, err := m.pki.issueClientCert(csr, a.state.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.nodes[a.state.NodeID].serial = serial
	m.mu.Unlock()

	if _, err := m.precheckResume(a.state.NodeID, string(oldCert)); err == nil {
		t.Fatal("the superseded certificate must be refused")
	}
	if _, err := m.precheckResume(a.state.NodeID, string(newCertPEM)); err != nil {
		t.Fatalf("the renewed certificate should be accepted: %v", err)
	}
}

func TestFinishedTaskIsPersistedAndReloaded(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a := enrollTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	connectAgent(t, m, a)

	var view taskView
	if status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec:  TaskSpec{Target: "https://example.com/", DurationSec: 30, TimeoutSec: 5},
		NodeIDs:   []string{a.state.NodeID},
		TotalConc: 2,
	}, &view); status != http.StatusOK {
		t.Fatalf("create task: %d", status)
	}
	m.applyResult(a.state.NodeID, TaskResult{TaskID: view.ID, Status: taskDone, Requests: 10, Bytes: 1024})

	path := filepath.Join(m.dir, "tasks", view.ID+".json")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task file %s was never written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}

	restarted, err := newMaster(masterOptions{Dir: m.dir})
	if err != nil {
		t.Fatalf("reload master: %v", err)
	}
	defer restarted.close()

	var found *taskView
	for i := range restarted.history {
		if restarted.history[i].ID == view.ID {
			found = &restarted.history[i]
		}
	}
	if found == nil {
		t.Fatal("a finished task should come back as history after a restart")
	}
	if found.State != taskDone || found.Totals.Requests != 10 {
		t.Fatalf("reloaded task mismatch: %s %+v", found.State, found.Totals)
	}
}

func TestNewJoinValidatesURLAndToken(t *testing.T) {
	m, _ := newTestMaster(t)
	for _, bad := range []string{"", "10.0.0.1:8443", "tcp://10.0.0.1:8443"} {
		if _, err := m.newJoin(bad); err == nil {
			t.Fatalf("newJoin(%q) should be rejected", bad)
		}
	}
	info, err := m.newJoin("http://10.0.0.1:8080/")
	if err != nil {
		t.Fatalf("plain http must be accepted: %v", err)
	}
	if info.URL != "http://10.0.0.1:8080" {
		t.Fatalf("trailing slash should be trimmed, got %q", info.URL)
	}
	decoded, err := decodeJoinString(info.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Token != info.Token || normalizeFingerprint(decoded.Fingerprint) != m.pki.fp {
		t.Fatal("join credential does not round trip")
	}
	if _, err := m.pki.verifyJoinToken(decoded.Token); err != nil {
		t.Fatalf("issued token must verify: %v", err)
	}
}

func TestParseListenSpec(t *testing.T) {
	cases := []struct {
		in, scheme, addr string
		ok               bool
	}{
		{"0.0.0.0:8443", "https", "0.0.0.0:8443", true},
		{"https://0.0.0.0:8443", "https", "0.0.0.0:8443", true},
		{"http://0.0.0.0:8080", "http", "0.0.0.0:8080", true},
		{"http://:8080/", "http", ":8080", true},
		{"ftp://0.0.0.0:21", "", "", false},
		{"0.0.0.0", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		scheme, addr, err := parseListenSpec(c.in)
		if c.ok {
			if err != nil || scheme != c.scheme || addr != c.addr {
				t.Errorf("parseListenSpec(%q) = %q,%q,%v", c.in, scheme, addr, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseListenSpec(%q) should fail", c.in)
		}
	}
}

// ---------- 测试辅助 ----------

func postRaw(t *testing.T, c *http.Client, url string, body any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}
