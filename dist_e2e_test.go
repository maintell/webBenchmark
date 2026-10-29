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

// TestDistributedEndToEnd 是最重要的一条：主控 + 两个从节点 + 一个真实目标，
// 覆盖入网、mTLS、派发、进度上报、结果汇总、任务收尾的全链路。
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
	a1 := newTestAgent(t, m, srv.URL, filepath.Join(dir, "a1"), "loadgen-1")
	a2 := newTestAgent(t, m, srv.URL, filepath.Join(dir, "a2"), "loadgen-2")

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

	var created taskView
	status := admin.call(http.MethodPost, "/api/v1/tasks", taskCreateRequest{
		TaskSpec: TaskSpec{
			Target:      target.URL + "/e2e",
			Proto:       "http1",
			DurationSec: 2,
			TimeoutSec:  5,
			XForwardFor: true,
		},
		NodeIDs:   []string{a1.state.NodeID, a2.state.NodeID},
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
	if final.Totals.Requests == 0 {
		t.Fatal("aggregated request count should be positive")
	}
	if final.Totals.Bytes == 0 {
		t.Fatal("aggregated byte count should be positive")
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
		if n.Requests == 0 {
			t.Fatalf("node %s reported no requests", n.NodeID)
		}
		if n.Name == "" {
			t.Fatalf("node %s should carry its display name", n.NodeID)
		}
	}
	if hits.Load() == 0 {
		t.Fatal("the target never received a request")
	}

	// 任务结束后节点应重新空闲，可以继续接新任务
	stopAgents()
}

// TestAgentKeepsIdentityAcrossRestart 验证节点身份落在数据目录里：
// 重启不需要重新申请令牌，换了主控也不会被静默接受。
func TestAgentKeepsIdentityAcrossRestart(t *testing.T) {
	m, srv := newTestMaster(t)
	dir := t.TempDir()
	first := newTestAgent(t, m, srv.URL, dir, "loadgen-1")

	second, err := newAgent(agentConfig{Dir: dir, URL: srv.URL, Name: "loadgen-1"})
	if err != nil {
		t.Fatalf("restart without a token must reuse the stored identity: %v", err)
	}
	if second.state.NodeID != first.state.NodeID {
		t.Fatalf("node id changed across restart: %s vs %s", first.state.NodeID, second.state.NodeID)
	}
	if status, body := agentPost(t, second, "/api/v1/agent/progress", Progress{TaskID: "task-none", Seq: 1}); status != http.StatusOK {
		t.Fatalf("reloaded agent should still authenticate, got %d %s", status, body)
	}

	// 已经加入过的主控关系不能被另一个主控覆盖
	_, otherSrv := newTestMaster(t)
	if _, err := newAgent(agentConfig{Dir: dir, URL: otherSrv.URL}); err == nil {
		t.Fatal("an enrolled data directory must not silently switch masters")
	}
}

// TestAgentRejectsWrongPinning 验证指纹不对时连接根本建立不起来。
func TestAgentRejectsWrongPinning(t *testing.T) {
	m, srv := newTestMaster(t)
	token, _, err := m.pki.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "00" + m.pki.fp[2:]
	if _, err := newAgent(agentConfig{
		Dir: t.TempDir(), URL: srv.URL, Name: "evil",
		Token: token, Fingerprint: wrong,
	}); err == nil {
		t.Fatal("an agent with the wrong CA fingerprint must not join")
	}
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
