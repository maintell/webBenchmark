package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stopSignal 返回一个已经放好原因的停止通道。
func stopSignal(reason string) <-chan string {
	ch := make(chan string, 1)
	if reason != "" {
		ch <- reason
	}
	return ch
}

// TestRunDistTaskReportsStoppedWhenCancelled 验证"被叫停"与"跑完"不会混为一谈。
func TestRunDistTaskReportsStoppedWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	spec := &TaskSpec{ID: "task-local", Target: srv.URL, DurationSec: 30, TimeoutSec: 5, PerNodeConc: 1}
	spec.normalize()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(600 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res := runDistTask(ctx, spec, 1, nil, make(chan string))
	if res.Status != taskStopped {
		t.Fatalf("cancelled task should be stopped, got %s (%s)", res.Status, res.Error)
	}
	if res.Requests == 0 {
		t.Fatal("it did send requests before being stopped")
	}
}

// TestRunDistTaskReportsErrorWhenNothingSent 验证"一个请求都没发出去"必须变成 error，
// 而不是一个全零但显示成功的结论。
func TestRunDistTaskReportsErrorWhenNothingSent(t *testing.T) {
	spec := &TaskSpec{ID: "task-bad", Target: "http://127.0.0.1:1/", DurationSec: 1, TimeoutSec: 1, PerNodeConc: 1}
	spec.normalize()

	res := runDistTask(context.Background(), spec, 1, nil, make(chan string))
	if res.Status != taskErrored {
		t.Fatalf("a task that sent nothing should be error, got %s", res.Status)
	}
	if res.Error == "" || res.Requests != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRunDistTaskHonoursDeadline 验证任务会自己到点收工，状态是 done。
func TestRunDistTaskHonoursDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	spec := &TaskSpec{ID: "task-dur", Target: srv.URL, DurationSec: 1, TimeoutSec: 5, PerNodeConc: 2}
	spec.normalize()

	start := time.Now()
	res := runDistTask(context.Background(), spec, 2, nil, make(chan string))
	if res.Status != taskDone {
		t.Fatalf("a task that reached its deadline should be done, got %s (%s)", res.Status, res.Error)
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("unexpected runtime %s", elapsed)
	}
	if res.Requests == 0 {
		t.Fatal("expected requests from the local run")
	}
}

// TestRunDistTaskStopsWhenToldTo 验证主控下发的 stop 能让任务停下，
// 并且 reason=deadline 时要算正常完成而不是中断。
func TestRunDistTaskStopsWhenToldTo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	spec := &TaskSpec{ID: "task-cb", Target: srv.URL, DurationSec: 30, TimeoutSec: 5, PerNodeConc: 1}
	spec.normalize()

	res := runDistTask(context.Background(), spec, 1, nil, stopSignal("stopped"))
	if res.Status != taskStopped {
		t.Fatalf("an operator stop should end as stopped, got %s", res.Status)
	}

	// deadline 停止要算正常完成：给任务一点时间真的发出请求，
	// 否则"零请求"会优先判成 error（那是另一条正确的分支）。
	late := make(chan string, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		late <- "deadline"
	}()
	res = runDistTask(context.Background(), spec, 1, nil, late)
	if res.Status != taskDone {
		t.Fatalf("a deadline stop should still count as done, got %s (%s)", res.Status, res.Error)
	}
}
