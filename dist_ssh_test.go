package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHInstallRequestNormalize(t *testing.T) {
	base := func() sshInstallRequest {
		return sshInstallRequest{
			Host: "10.0.0.11", User: "root", Password: "pw",
			MasterURL: "https://10.0.0.1:8443",
		}
	}
	ok := base()
	if err := ok.normalize(); err != nil {
		t.Fatalf("baseline should pass: %v", err)
	}
	if ok.Port != 22 || ok.Dir != "$HOME/.webBenchmark-agent" || ok.NodeName != "host" {
		t.Fatalf("defaults not applied: %+v", ok)
	}

	bad := []struct {
		name string
		edit func(*sshInstallRequest)
	}{
		{"no host", func(r *sshInstallRequest) { r.Host = "" }},
		{"host with space", func(r *sshInstallRequest) { r.Host = "10 0 0 11" }},
		{"host with slash", func(r *sshInstallRequest) { r.Host = "10.0.0.11/etc" }},
		{"bad port", func(r *sshInstallRequest) { r.Port = 99999 }},
		{"user injection", func(r *sshInstallRequest) { r.User = "root; rm -rf /" }},
		{"master not http", func(r *sshInstallRequest) { r.MasterURL = "ftp://x" }},
		{"dir injection", func(r *sshInstallRequest) { r.Dir = "$HOME/x; reboot" }},
		{"dir with space", func(r *sshInstallRequest) { r.Dir = "/opt/my app" }},
		{"name injection", func(r *sshInstallRequest) { r.NodeName = "a b" }},
		{"no credentials", func(r *sshInstallRequest) { r.Password = "" }},
	}
	for _, c := range bad {
		req := base()
		c.edit(&req)
		if err := req.normalize(); err == nil {
			t.Errorf("%s should be rejected", c.name)
		}
	}
}

func TestPlatformMatches(t *testing.T) {
	// 用当前编译平台构造一个必须匹配的 uname，验证映射本身；
	// 再确认明显不相干的名字一律拒绝。
	current := map[string]string{
		"linux/amd64":   "Linux x86_64",
		"linux/arm64":   "Linux aarch64",
		"linux/arm":     "Linux armv7l",
		"freebsd/amd64": "FreeBSD amd64",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	if current != "" && !platformMatches(current) {
		t.Fatalf("platformMatches(%q) should be true on %s/%s", current, runtime.GOOS, runtime.GOARCH)
	}
	for _, bad := range []string{"", "SunOS sun4u", "Linux", "Darwin arm64"} {
		if runtime.GOOS == "darwin" && bad == "Darwin arm64" {
			continue
		}
		if platformMatches(bad) {
			t.Errorf("platformMatches(%q) should be false", bad)
		}
	}
}

func TestKnownHostsStoreAndTOFU(t *testing.T) {
	m, _ := newTestMaster(t)
	hostport := "10.0.0.11:22"
	if got := m.lookupHostKey(hostport); got != "" {
		t.Fatalf("empty store should return nothing, got %q", got)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(key)

	// 未知主机：默认必须报"需要确认"，而不是默默接受
	cb := m.hostKeyCallback(hostport, "", false)
	err = cb(hostport, nil, key)
	fpErr, ok := err.(*sshFingerprintError)
	if !ok || fpErr.fingerprint != fingerprint {
		t.Fatalf("unknown host should raise a fingerprint error, got %v", err)
	}

	// 带正确指纹确认一次后应当记住
	if err := m.hostKeyCallback(hostport, fingerprint, false)(hostport, nil, key); err != nil {
		t.Fatalf("accepting the fingerprint should succeed: %v", err)
	}
	if got := m.lookupHostKey(hostport); got != fingerprint {
		t.Fatalf("fingerprint not recorded: %q", got)
	}

	// 记住之后指纹对得上就直接通过，不需要再确认
	if err := m.hostKeyCallback(hostport, "", false)(hostport, nil, key); err != nil {
		t.Fatalf("recorded fingerprint should be accepted: %v", err)
	}

	// 换一把钥匙（相当于中间人）必须被拒，且不能覆盖记录
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ssh.NewPublicKey(otherPub)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.hostKeyCallback(hostport, "", false)(hostport, nil, otherKey); err == nil {
		t.Fatal("a changed host key must be rejected")
	}
	if !strings.Contains(m.lookupHostKey(hostport), strings.TrimPrefix(fingerprint, "SHA256:")) {
		t.Fatal("the stored fingerprint must not be overwritten by a mismatching key")
	}

	// insecure 只用于显式放弃校验的场景
	other := "10.0.0.12:22"
	if err := m.hostKeyCallback(other, "", true)(other, nil, otherKey); err != nil {
		t.Fatalf("insecure mode should accept and remember: %v", err)
	}
	if m.lookupHostKey(other) == "" {
		t.Fatal("insecure mode should still record the key")
	}
}
