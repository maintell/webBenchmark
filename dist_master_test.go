package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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
// 证书与客户端校验规则和生产环境完全一致：只信任自建 CA，且允许浏览器不带客户端证书。
func newTestMaster(t *testing.T) (*master, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	hash, _, err := loadOrCreateAdminHash(dir, testAdminPassword)
	if err != nil {
		t.Fatalf("admin hash: %v", err)
	}
	m, err := newMaster(masterOptions{
		Dir:       dir,
		JoinTTL:   15 * time.Minute,
		AdminHash: hash,
		PollWait:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newMaster: %v", err)
	}
	srv := httptest.NewUnstartedServer(m.handler())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{m.serverCert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    m.pki.caPool(),
	}
	srv.StartTLS()
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(func() {
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

func newTestAgent(t *testing.T, m *master, base, dir, name string) *agent {
	t.Helper()
	token, _, err := m.pki.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAgent(agentConfig{
		Dir:         dir,
		URL:         base,
		Name:        name,
		Token:       token,
		Fingerprint: m.pki.fp,
	})
	if err != nil {
		t.Fatalf("enroll %s: %v", name, err)
	}
	return a
}

func agentPost(t *testing.T, a *agent, path string, body any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.endpoint(path), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		t.Fatalf("agent %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestEnrollRejectsBadToken(t *testing.T) {
	m, srv := newTestMaster(t)

	_, csr, err := newAgentCSR("evil")
	if err != nil {
		t.Fatal(err)
	}
	// 指纹正确的客户端：说明这一步被拒的原因是令牌，而不是 TLS。
	pinned := newPinnedClient(t, m)
	status, body := postRaw(t, pinned, srv.URL+"/api/v1/enroll", enrollRequest{
		Token: "wb1.bogus.sig", CSR: csr, Name: "evil",
	})
	if status != http.StatusForbidden {
		t.Fatalf("bad token should be 403, got %d %s", status, body)
	}
	if !strings.Contains(string(body), "令牌") {
		t.Fatalf("rejection should mention the token: %s", body)
	}
}

func TestJoinTokenIsSingleUse(t *testing.T) {
	m, srv := newTestMaster(t)
	token, _, err := m.pki.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	dir1 := t.TempDir()
	if _, err := newAgent(agentConfig{Dir: dir1, URL: srv.URL, Name: "a1", Token: token, Fingerprint: m.pki.fp}); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	dir2 := t.TempDir()
	_, err = newAgent(agentConfig{Dir: dir2, URL: srv.URL, Name: "a2", Token: token, Fingerprint: m.pki.fp})
	if err == nil {
		t.Fatal("the same join token must not enroll a second node")
	}
	if !strings.Contains(err.Error(), "已被使用") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentAPIRefusesMissingClientCert(t *testing.T) {
	m, srv := newTestMaster(t)
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    m.pki.caPool(),
		ServerName: distServerName,
		MinVersion: tls.VersionTLS13,
	}}}
	status, body := postRaw(t, plain, srv.URL+"/api/v1/agent/poll", pollRequest{Proto: distProtoVersion})
	if status != http.StatusUnauthorized {
		t.Fatalf("poll without a client cert should be 401, got %d %s", status, body)
	}
}

func TestEnrolledAgentCanPoll(t *testing.T) {
	m, srv := newTestMaster(t)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "loadgen-1")

	// 没有命令时，长轮询应当一直挂到窗口结束才返回空。
	started := time.Now()
	status, body := agentPost(t, a, "/api/v1/agent/poll", pollRequest{Proto: distProtoVersion, Version: version})
	if status != http.StatusOK {
		t.Fatalf("poll returned %d %s", status, body)
	}
	if elapsed := time.Since(started); elapsed < m.pollWait/2 {
		t.Fatalf("poll returned after %s, expected it to wait about %s", elapsed, m.pollWait)
	}
	var empty pollResponse
	if err := json.Unmarshal(body, &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Command != nil {
		t.Fatalf("no command was queued, got %+v", empty.Command)
	}

	// 排一条命令后，长轮询必须被立刻唤醒，而不是等满窗口。
	m.mu.Lock()
	m.enqueueLocked(a.state.NodeID, Command{Kind: cmdStop, TaskID: "task-abc"})
	m.mu.Unlock()

	started = time.Now()
	status, body = agentPost(t, a, "/api/v1/agent/poll", pollRequest{Proto: distProtoVersion})
	if status != http.StatusOK {
		t.Fatalf("poll returned %d %s", status, body)
	}
	if elapsed := time.Since(started); elapsed > m.pollWait {
		t.Fatalf("queued command should wake the poll immediately, took %s", elapsed)
	}
	var withCmd pollResponse
	if err := json.Unmarshal(body, &withCmd); err != nil {
		t.Fatal(err)
	}
	if withCmd.Command == nil || withCmd.Command.Kind != cmdStop || withCmd.Command.TaskID != "task-abc" {
		t.Fatalf("unexpected command: %+v", withCmd.Command)
	}
}

func TestPollRejectsProtocolMismatch(t *testing.T) {
	m, srv := newTestMaster(t)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "loadgen-1")

	status, body := agentPost(t, a, "/api/v1/agent/poll", pollRequest{Proto: distProtoVersion + 1})
	if status != http.StatusConflict {
		t.Fatalf("mixed protocol versions must be rejected with 409, got %d %s", status, body)
	}
	if !strings.Contains(string(body), "协议版本") {
		t.Fatalf("error should explain the version mismatch: %s", body)
	}
}

func TestRevokedNodeIsRejected(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "loadgen-1")

	if status, body := agentPost(t, a, "/api/v1/agent/poll", pollRequest{Proto: distProtoVersion}); status != http.StatusOK {
		t.Fatalf("healthy node poll should be 200, got %d %s", status, body)
	}

	if status := admin.call(http.MethodPost, "/api/v1/nodes/"+a.state.NodeID+"/revoke", nil, nil); status != http.StatusOK {
		t.Fatalf("revoke failed: %d", status)
	}
	status, body := agentPost(t, a, "/api/v1/agent/poll", pollRequest{Proto: distProtoVersion})
	if status != http.StatusForbidden {
		t.Fatalf("revoked node should be 403, got %d %s", status, body)
	}
	if !strings.Contains(string(body), "吊销") {
		t.Fatalf("revoked node should be told why: %s", body)
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
	// 不带 CSRF 头的写操作必须被拒
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
	// 即使密码正确，封锁期内也不能登录
	status, _ = postRaw(t, anon, srv.URL+"/api/v1/login", map[string]string{"password": testAdminPassword})
	if status != http.StatusTooManyRequests {
		t.Fatalf("blocked source must stay blocked, got %d", status)
	}
}

func TestCreateTaskSplitsConcurrencyAndRejectsBusyNodes(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a1 := newTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	a2 := newTestAgent(t, m, srv.URL, t.TempDir(), "a2")

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
	if view.TotalConc != 10 {
		t.Fatalf("total concurrency should be 10, got %d", view.TotalConc)
	}
	if len(view.Nodes) != 2 {
		t.Fatalf("expected 2 shards, got %d", len(view.Nodes))
	}

	// 同一批节点不能并行跑第二条任务
	status = admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec:  TaskSpec{Target: "https://example.com/", DurationSec: 30, TimeoutSec: 5},
		NodeIDs:   []string{a1.state.NodeID},
		TotalConc: 2,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("busy node should be rejected with 400, got %d", status)
	}

	// 收尾，避免后台的 watchTask 一直跑
	m.mu.Lock()
	task := m.tasks[view.ID]
	m.mu.Unlock()
	if task == nil {
		t.Fatal("task should be registered")
	}
	for _, id := range []string{a1.state.NodeID, a2.state.NodeID} {
		m.applyResult(id, TaskResult{TaskID: view.ID, Status: taskDone, Requests: 1})
	}
	m.mu.Lock()
	state := task.state
	m.mu.Unlock()
	if state != taskDone {
		t.Fatalf("task should be done after every shard reported, got %s", state)
	}
}

func TestProgressAggregation(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a1 := newTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	a2 := newTestAgent(t, m, srv.URL, t.TempDir(), "a2")

	var view taskView
	if status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec:  TaskSpec{Target: "https://example.com/", DurationSec: 60, TimeoutSec: 5, XForwardFor: true},
		NodeIDs:   []string{a1.state.NodeID, a2.state.NodeID},
		TotalConc: 4,
	}, &view); status != http.StatusOK {
		t.Fatalf("create task: %d", status)
	}

	// 第一帧只建立基线，第二帧才开始算速率
	m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 1, Requests: 100, Bytes: 1000})
	m.applyProgress(a2.state.NodeID, Progress{TaskID: view.ID, Seq: 1, Requests: 50, Bytes: 500})
	time.Sleep(250 * time.Millisecond)
	stop, byDeadline, known := m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 200, Failed: 3, Bytes: 2000})
	if !known || stop || byDeadline {
		t.Fatalf("progress should be accepted while running (known=%v stop=%v deadline=%v)", known, stop, byDeadline)
	}
	m.applyProgress(a2.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 150, Failed: 1, Bytes: 1500})

	// 乱序/重复的包必须被忽略，否则速率会算成负数
	if _, _, known := m.applyProgress(a1.state.NodeID, Progress{TaskID: view.ID, Seq: 2, Requests: 1}); !known {
		t.Fatal("duplicate seq should still be treated as a known task")
	}

	var live taskView
	if status := admin.call(http.MethodGet, "/api/v1/tasks/"+view.ID, nil, &live); status != http.StatusOK {
		t.Fatalf("get task: %d", status)
	}
	if live.Totals.Requests != 350 {
		t.Fatalf("requests should be summed across shards, got %d", live.Totals.Requests)
	}
	if live.Totals.Failed != 4 {
		t.Fatalf("failures should be summed, got %d", live.Totals.Failed)
	}
	if live.Totals.Bytes != 3500 {
		t.Fatalf("bytes should be summed, got %d", live.Totals.Bytes)
	}
	if live.Totals.RPS <= 0 {
		t.Fatalf("aggregate rps should be positive, got %f", live.Totals.RPS)
	}
	if len(live.Series) == 0 {
		t.Fatal("series should collect samples for the chart")
	}
	if len(live.Nodes) != 2 {
		t.Fatalf("expected two shards, got %d", len(live.Nodes))
	}

	// 停止后，迟到的进度包会让节点立刻收到 stop
	if status := admin.call(http.MethodPost, "/api/v1/tasks/"+view.ID+"/stop", nil, nil); status != http.StatusOK {
		t.Fatalf("stop: %d", status)
	}
	if stop, byDeadline, known := m.applyProgress(a2.state.NodeID, Progress{TaskID: view.ID, Seq: 3}); !known || !stop {
		t.Fatalf("stopped task must tell the agent to stop (known=%v stop=%v)", known, stop)
	} else if byDeadline {
		t.Fatal("a manual stop must not be reported as the task deadline")
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

func TestUnknownTaskProgressTellsAgentToStop(t *testing.T) {
	m, srv := newTestMaster(t)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "a1")

	status, body := agentPost(t, a, "/api/v1/agent/progress", Progress{TaskID: "task-nope", Seq: 1})
	if status != http.StatusOK {
		t.Fatalf("progress for an unknown task should still be 200, got %d %s", status, body)
	}
	var out struct {
		Known bool `json:"known"`
		Stop  bool `json:"stop"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Known || !out.Stop {
		t.Fatalf("unknown task must be reported as unknown+stop, got %s", body)
	}
}

// ---------- 测试辅助 ----------

// newPinnedClient 是"没有 CA 文件、只有指纹"的首次入网客户端，
// 与从节点 bootstrap 用的验证逻辑完全一致。
func newPinnedClient(t *testing.T, m *master) *http.Client {
	t.Helper()
	fp := m.pki.fp
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         distServerName,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			return verifyPinnedChain(raw, fp)
		},
	}}}
}

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

// postWithClient 用指定客户端发一个 agent 请求，用于"旧证书还能不能用"这类断言。
func postWithClient(t *testing.T, c *http.Client, url string, body any) (int, []byte) {
	t.Helper()
	return postRaw(t, c, url, body)
}

func clientWithCert(t *testing.T, m *master, certPEM, keyPEM []byte) *http.Client {
	t.Helper()
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      m.pki.caPool(),
		ServerName:   distServerName,
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS13,
	}}}
}

// TestRenewedCertificateReplacesTheOldOne 验证换发是原子替换：
// 新证书一签发，旧证书立刻不能再用，而不是新旧并存一段时间。
func TestRenewedCertificateReplacesTheOldOne(t *testing.T) {
	m, srv := newTestMaster(t)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "a1")
	oldClient := a.client()

	if status, body := postWithClient(t, oldClient, srv.URL+"/api/v1/agent/progress", Progress{TaskID: "task-x", Seq: 1}); status != http.StatusOK {
		t.Fatalf("old cert should work before renewal, got %d %s", status, body)
	}

	key, csr, err := newAgentCSR("a1")
	if err != nil {
		t.Fatal(err)
	}
	var resp renewResponse
	if err := a.postJSON(context.Background(), oldClient, a.endpoint("/api/v1/agent/renew"), renewRequest{CSR: csr}, &resp); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if resp.NodeID != a.state.NodeID {
		t.Fatalf("renew must keep the node identity, got %s", resp.NodeID)
	}

	if status, _ := postWithClient(t, oldClient, srv.URL+"/api/v1/agent/progress", Progress{TaskID: "task-x", Seq: 2}); status != http.StatusForbidden {
		t.Fatalf("the superseded certificate must be refused, got %d", status)
	}

	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	fresh := clientWithCert(t, m, []byte(resp.CertPEM), keyPEM)
	if status, body := postWithClient(t, fresh, srv.URL+"/api/v1/agent/progress", Progress{TaskID: "task-x", Seq: 3}); status != http.StatusOK {
		t.Fatalf("the renewed certificate should work, got %d %s", status, body)
	}
}

// TestFinishedTaskIsPersistedAndReloaded 验证结论落盘并在重启后可读。
func TestFinishedTaskIsPersistedAndReloaded(t *testing.T) {
	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)
	a := newTestAgent(t, m, srv.URL, t.TempDir(), "a1")

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
	if found.State != taskDone {
		t.Fatalf("reloaded state should be done, got %s", found.State)
	}
	if found.Totals.Requests != 10 {
		t.Fatalf("reloaded totals should match, got %d", found.Totals.Requests)
	}
}

func TestNewJoinValidatesURLAndToken(t *testing.T) {
	m, _ := newTestMaster(t)
	for _, bad := range []string{"", "http://10.0.0.1:8443", "10.0.0.1:8443"} {
		if _, err := m.newJoin(bad); err == nil {
			t.Fatalf("newJoin(%q) should be rejected", bad)
		}
	}
	info, err := m.newJoin("https://10.0.0.1:8443/")
	if err != nil {
		t.Fatalf("newJoin: %v", err)
	}
	if info.URL != "https://10.0.0.1:8443" {
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
