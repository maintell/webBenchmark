package main

import (
	"strings"
	"testing"
	"time"
)

func TestSplitPerNode(t *testing.T) {
	cases := []struct {
		total, nodes, want int
	}{
		{64, 4, 16},
		{64, 3, 22},      // 向上取整，保证总并发不低于请求值
		{1, 4, 1},        // 节点比并发多时每台至少 1 条
		{0, 2, 1},        // 非法总并发退化成 1
		{10, 0, 10},      // 节点数为 0 时按 1 台算
		{100000, 1, 256}, // 单节点上限夹住
	}
	for _, c := range cases {
		if got := splitPerNode(c.total, c.nodes); got != c.want {
			t.Errorf("splitPerNode(%d,%d)=%d want %d", c.total, c.nodes, got, c.want)
		}
	}
}

func TestSanitizeNodeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"loadgen-1", "loadgen-1"},
		{"  负载机 A  ", "负载机 A"},
		{"line\nbreak\ttab", "line break tab"},
		{"bad\x00\x07chars", "badchars"},
		{strings.Repeat("x", 40), strings.Repeat("x", distMaxNodeNameLen)},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeNodeName(c.in); got != c.want {
			t.Errorf("sanitizeNodeName(%q)=%q want %q", c.in, got, c.want)
		}
	}
	// 中文按 rune 截断，不能把多字节字符切坏。
	got := sanitizeNodeName(strings.Repeat("压", 40))
	if len([]rune(got)) != distMaxNodeNameLen {
		t.Errorf("rune truncate got %d runes", len([]rune(got)))
	}
}

func TestValidHeaderName(t *testing.T) {
	good := []string{"X-Test", "cookie", "a", "X_Forwarded-For.1", "!#$%&'*+-.^_`|~"}
	for _, s := range good {
		if !validHeaderName(s) {
			t.Errorf("validHeaderName(%q) should be true", s)
		}
	}
	bad := []string{"", "bad name", "X-Test:", "a\r\nb", strings.Repeat("x", 65)}
	for _, s := range bad {
		if validHeaderName(s) {
			t.Errorf("validHeaderName(%q) should be false", s)
		}
	}
}

func TestTaskSpecValidate(t *testing.T) {
	base := func() TaskSpec {
		spec := TaskSpec{
			Target:      "https://example.com/x",
			Proto:       "http1",
			DurationSec: 30,
			PerNodeConc: 8,
			TimeoutSec:  30,
		}
		spec.normalize()
		return spec
	}

	baseline := base()
	if err := baseline.validate(); err != nil {
		t.Fatalf("baseline spec should validate: %v", err)
	}

	cases := []struct {
		name string
		edit func(*TaskSpec)
	}{
		{"empty target", func(s *TaskSpec) { s.Target = "" }},
		{"ftp scheme", func(s *TaskSpec) { s.Target = "ftp://example.com" }},
		{"no host", func(s *TaskSpec) { s.Target = "https://" }},
		{"bad proto", func(s *TaskSpec) { s.Proto = "gopher" }},
		{"zero duration", func(s *TaskSpec) { s.DurationSec = 0 }},
		{"too long duration", func(s *TaskSpec) { s.DurationSec = distMaxDurationSec + 1 }},
		{"zero conc", func(s *TaskSpec) { s.PerNodeConc = 0 }},
		{"huge conc", func(s *TaskSpec) { s.PerNodeConc = distMaxNodeConcurrency + 1 }},
		{"negative timeout", func(s *TaskSpec) { s.TimeoutSec = -1 }},
		{"header injection", func(s *TaskSpec) {
			s.Headers = []HeaderPair{{Name: "X-Test\r\nX-Evil", Value: "1"}}
		}},
		{"too many headers", func(s *TaskSpec) {
			for i := 0; i <= distMaxHeadersPerTask; i++ {
				s.Headers = append(s.Headers, HeaderPair{Name: "X-T", Value: "v"})
			}
		}},
		{"bad family", func(s *TaskSpec) { s.Family = "ipv7" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := base()
			c.edit(&spec)
			if err := spec.validate(); err == nil {
				t.Fatalf("%s should fail validation", c.name)
			}
		})
	}
}

func TestTaskSpecNormalizeKeepsExplicitZero(t *testing.T) {
	// 0 秒超时是"不设超时"，不能被 normalize 悄悄改成 30 秒：
	// 那会让一个显式的决定变成另一个测量口径。
	spec := TaskSpec{Target: "https://example.com", TimeoutSec: 0, PerNodeConc: 4}
	spec.normalize()
	if spec.TimeoutSec != 0 {
		t.Fatalf("TimeoutSec=0 must stay 0, got %d", spec.TimeoutSec)
	}
	if spec.DurationSec != 60 {
		t.Fatalf("duration default should be 60, got %d", spec.DurationSec)
	}
	if spec.ID == "" || !strings.HasPrefix(spec.ID, "task-") {
		t.Fatalf("task id should be generated, got %q", spec.ID)
	}
}

func TestJoinStringRoundTrip(t *testing.T) {
	info := joinInfo{
		V:           distProtoVersion,
		URL:         "https://10.0.0.1:8443",
		Token:       "wb1.abc.def",
		Fingerprint: strings.Repeat("ab", 32),
		ExpiresAt:   time.Now().Add(time.Hour).UTC(),
	}
	encoded := info.encode()
	if !strings.HasPrefix(encoded, "wb1_") {
		t.Fatalf("join string should carry the wb1_ prefix, got %q", encoded)
	}

	for _, input := range []string{
		encoded,
		"webBenchmark -join " + encoded,
		"  " + encoded + "  ",
	} {
		got, err := decodeJoinString(input)
		if err != nil {
			t.Fatalf("decode(%q): %v", input, err)
		}
		if got.URL != info.URL || got.Token != info.Token || got.Fingerprint != info.Fingerprint {
			t.Fatalf("round trip mismatch: %+v", got)
		}
	}

	if _, err := decodeJoinString("wb1_not-base64"); err == nil {
		t.Fatal("garbage join string must be rejected")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	want := strings.Repeat("ab", 32)
	for _, in := range []string{
		want,
		strings.ToUpper(want),
		"sha256:" + want,
		groupFingerprint(want),
	} {
		if got := normalizeFingerprint(in); got != want {
			t.Errorf("normalizeFingerprint(%q)=%q want %q", in, got, want)
		}
	}
}

func TestShardTerminal(t *testing.T) {
	for _, s := range []string{shardDone, shardStopped, shardLost, shardErrored} {
		if !shardTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []string{shardAssigned, shardRunning, ""} {
		if shardTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
