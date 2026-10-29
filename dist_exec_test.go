package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

	res := runDistTask(ctx, spec, 1, nil)
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

	res := runDistTask(context.Background(), spec, 1, nil)
	if res.Status != taskErrored {
		t.Fatalf("a task that sent nothing should be error, got %s", res.Status)
	}
	if res.Error == "" {
		t.Fatal("the error status should carry a reason")
	}
	if res.Requests != 0 {
		t.Fatalf("expected zero successful requests, got %d", res.Requests)
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
	res := runDistTask(context.Background(), spec, 2, nil)
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

// TestRunDistTaskStopsWhenSampleSaysSo 验证主控可以在某一帧上喊停。
func TestRunDistTaskStopsWhenSampleSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	spec := &TaskSpec{ID: "task-cb", Target: srv.URL, DurationSec: 30, TimeoutSec: 5, PerNodeConc: 1}
	spec.normalize()

	samples := 0
	res := runDistTask(context.Background(), spec, 1, func(p Progress) (bool, bool) {
		samples++
		if samples >= 2 {
			return true, false // 主控/控制台要求停
		}
		return false, false
	})
	if res.Status != taskStopped {
		t.Fatalf("a callback stop should end as stopped, got %s", res.Status)
	}
	if samples < 2 {
		t.Fatalf("expected at least two samples, got %d", samples)
	}
}
