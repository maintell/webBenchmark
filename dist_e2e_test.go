package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDistributedEndToEnd 覆盖全链路：入网 -> 加密握手 -> 派发 -> 进度 -> 结果 -> 收尾。
func TestDistributedEndToEnd(t *testing.T) {
	var hits atomic.Int64
	payload := bytes.Repeat([]byte("x"), 4096)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(payload)
	}))
	defer target.Close()

	m, srv := newTestMaster(t)
	admin := newTestAdmin(t, m, srv.URL)

	dir := t.TempDir()
	a1 := enrollTestAgent(t, m, srv.URL, filepath.Join(dir, "a1"), "loadgen-1")
	a2 := enrollTestAgent(t, m, srv.URL, filepath.Join(dir, "a2"), "loadgen-2")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, a := range []*agent{a1, a2} {
		wg.Add(1)
		go func(a *agent) {
			defer wg.Done()
			_ = a.run(ctx)
		}(a)
	}
	stopped := false
	stopAgents := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		wg.Wait()
	}
	defer stopAgents()

	waitNodesOnline(t, m, 2, 10*time.Second)

	// 从主控侧取节点 ID：agent 的 state 是它自己协程写的，
	// 测试协程直接读会和入网流程撞上（-race 会报）。
	ids := onlineNodeIDs(t, m)
	nodeA, nodeB := ids["loadgen-1"], ids["loadgen-2"]
	if nodeA == "" || nodeB == "" {
		t.Fatalf("both agents should be online, got %v", ids)
	}

	var created taskView
	status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec: TaskSpec{
			Target:      target.URL + "/e2e",
			Proto:       "http1",
			DurationSec: 2,
			TimeoutSec:  5,
			XForwardFor: true,
		},
		NodeIDs:   []string{nodeA, nodeB},
		TotalConc: 4,
	}, &created)
	if status != http.StatusOK {
		t.Fatalf("create task failed: %d", status)
	}
	if created.PerNodeConc != 2 {
		t.Fatalf("4 connections over 2 nodes should be 2 each, got %d", created.PerNodeConc)
	}

	final := waitTaskFinal(t, admin, created.ID, 30*time.Second)

	if final.State != taskDone {
		t.Fatalf("task should finish as done, got %s (nodes: %+v)", final.State, final.Nodes)
	}
	if final.Totals.Requests == 0 || final.Totals.Bytes == 0 {
		t.Fatalf("aggregated totals should be positive: %+v", final.Totals)
	}
	if final.Totals.PeakRPS <= 0 {
		t.Fatalf("peak rps should be recorded, got %f", final.Totals.PeakRPS)
	}
	if len(final.Series) == 0 {
		t.Fatal("series should carry samples for the console chart")
	}
	if len(final.Nodes) != 2 {
		t.Fatalf("expected two node rows, got %d", len(final.Nodes))
	}
	for _, n := range final.Nodes {
		if n.State != shardDone {
			t.Fatalf("node %s should be done, got %s (%s)", n.NodeID, n.State, n.LastError)
		}
		if n.Name == "" {
			t.Fatalf("node %s should carry its display name", n.NodeID)
		}
	}
	if hits.Load() == 0 {
		t.Fatal("the target never received a request")
	}

	stopAgents()
}

// TestAgentKeepsIdentityAcrossRestart 验证身份落在数据目录里：
// 重启不需要重新申请令牌，换了主控也不会被静默接受。
func TestAgentKeepsIdentityAcrossRestart(t *testing.T) {
	m, srv := newTestMaster(t)
	dir := t.TempDir()
	first := enrollTestAgent(t, m, srv.URL, dir, "loadgen-1")
	connectAgent(t, m, first)

	second, err := newAgent(agentConfig{Dir: dir, URL: srv.URL, Name: "loadgen-1"})
	if err != nil {
		t.Fatalf("restart without a token must reuse the stored identity: %v", err)
	}
	if second.state.NodeID != first.state.NodeID {
		t.Fatalf("node id changed across restart: %s vs %s", first.state.NodeID, second.state.NodeID)
	}
	connectAgent(t, m, second)

	_, otherSrv := newTestMaster(t)
	if _, err := newAgent(agentConfig{Dir: dir, URL: otherSrv.URL}); err == nil {
		t.Fatal("an enrolled data directory must not silently switch masters")
	}
}

// TestAgentRejectsWrongPinning 验证指纹不对时握手中断。
func TestAgentRejectsWrongPinning(t *testing.T) {
	m, srv := newTestMaster(t)
	token, _, err := m.pki.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "00" + m.pki.fp[2:]
	a, err := newAgent(agentConfig{
		Dir: t.TempDir(), URL: srv.URL, Name: "evil",
		Token: token, Fingerprint: wrong,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.connect(context.Background()); err == nil {
		t.Fatal("an agent with the wrong CA fingerprint must not join")
	}
	// 具体在哪一层被拒取决于部署：wss 下 TLS 层先拦（证书不受信），
	// ws 下则由应用层握手比对指纹。两种都算"没能加入"，关键是没有身份落地。
	if a.hasIdentity() {
		t.Fatal("a failed join must not persist an identity")
	}
}

// onlineNodeIDs 返回 name -> nodeID（只统计已连接、未吊销的节点）。
func onlineNodeIDs(t *testing.T, m *master) map[string]string {
	t.Helper()
	out := make(map[string]string)
	m.mu.Lock()
	for id, n := range m.nodes {
		if n.conn != nil && !n.revoked {
			out[n.name] = id
		}
	}
	m.mu.Unlock()
	return out
}

func waitNodesOnline(t *testing.T, m *master, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(onlineNodeIDs(t, m)) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d nodes online within %s", want, timeout)
}

func waitTaskFinal(t *testing.T, admin *testAdmin, id string, timeout time.Duration) taskView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last taskView
	for time.Now().Before(deadline) {
		if status := admin.call(http.MethodGet, "/api/v1/tasks/"+id, nil, &last); status != http.StatusOK {
			t.Fatalf("get task %s: %d", id, status)
		}
		if last.State != taskRunning {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish in %s (state=%s, nodes=%+v)", id, timeout, last.State, last.Nodes)
	return last
}
